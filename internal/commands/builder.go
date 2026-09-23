package commands

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tidwall/sjson"

	"wherobots/cli/internal/config"
	"wherobots/cli/internal/executor"
	"wherobots/cli/internal/hints"
	"wherobots/cli/internal/spec"
)

var persistentFlagNames = map[string]struct{}{
	"json":    {},
	"query":   {},
	"q":       {},
	"dry-run": {},
	"tree":    {},
	"yes":     {},
	"y":       {},
}

type parameterBinding struct {
	Param    spec.Parameter
	FlagName string
	JSON     bool
}

type bodyFieldBinding struct {
	Field    spec.BodyField
	FlagName string
	JSON     bool
}

type bodyBinding struct {
	SchemaType string
	BodyFlag   string
	BodyJSON   bool
	Fields     []bodyFieldBinding
}

func BuildRootCommand(cfg config.Config, creds executor.Credentials, runtimeSpec *spec.RuntimeSpec) *cobra.Command {
	flags := &GlobalFlags{}
	client := &http.Client{Timeout: cfg.HTTPTimeout}
	operationByCommand := map[*cobra.Command]*spec.Operation{}
	resourceCommands := map[string]*cobra.Command{}

	var root *cobra.Command
	printTree := func(cmd *cobra.Command) error {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), RenderTree(root))
		return err
	}

	root = &cobra.Command{
		Use:           cfg.AppName,
		SilenceUsage:  true,
		SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if flags.Tree {
				return printTree(cmd)
			}
			return cmd.Help()
		},
	}

	root.PersistentFlags().StringVar(&flags.JSONBody, "json", "", "raw JSON request body (overrides body field flags)")
	root.PersistentFlags().StringArrayVarP(&flags.Query, "query", "q", nil, "query pair (key=value), repeatable")
	root.PersistentFlags().BoolVar(&flags.DryRun, "dry-run", false, "print curl equivalent without executing request")
	root.PersistentFlags().BoolVar(&flags.Tree, "tree", false, "print available command tree")
	root.PersistentFlags().BoolVarP(&flags.Yes, "yes", "y", false, "skip confirmation prompt (for CI/scripts)")

	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return hints.Wrap(findOperationContext(operationByCommand, cmd), err)
	})

	apiCmd := &cobra.Command{
		Use:           "api",
		Short:         "Direct API access to Wherobots services",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if flags.Tree {
				return printTree(cmd)
			}
			return cmd.Help()
		},
	}
	root.AddCommand(apiCmd)

	for _, op := range runtimeSpec.Operations {
		if op.Excluded {
			continue
		}
		parent := ensureResourceHierarchy(apiCmd, resourceCommands, PathToResourceSegments(op.Path))
		verb := uniqueVerbName(parent, ChooseVerb(op))
		op.Verb = verb
		op.CommandPath = append(PathToResourceSegments(op.Path), verb)

		methodCommand := buildOperationCommand(op, flags, creds, runtimeSpec, client, printTree)
		operationByCommand[methodCommand] = op
		parent.AddCommand(methodCommand)
	}

	addJobsCustomCommands(root, cfg, creds, runtimeSpec, client, flags)
	addFilesCommands(root, cfg, creds, runtimeSpec, client, flags)

	return root
}

func buildOperationCommand(
	op *spec.Operation,
	flags *GlobalFlags,
	creds executor.Credentials,
	runtimeSpec *spec.RuntimeSpec,
	client *http.Client,
	printTree func(cmd *cobra.Command) error,
) *cobra.Command {
	cmd := &cobra.Command{
		Use:           op.Verb,
		Short:         buildOperationShort(op),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return hints.Wrap(op, fmt.Errorf("unexpected positional args %v; use named flags", args))
			}
			return nil
		},
	}

	usedFlagNames := copyFlagSet(persistentFlagNames)
	pathBindings := registerPathParameterFlags(cmd, op, usedFlagNames)
	queryBindings := registerQueryParameterFlags(cmd, op, usedFlagNames)
	bodyFlags := registerBodyFlags(cmd, op, usedFlagNames)
	cmd.Long = buildOperationHelp(op, pathBindings, queryBindings, bodyFlags)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if flags.Tree {
			return printTree(cmd)
		}

		if !flags.DryRun && isWriteMethod(op.Method) && !flags.Yes {
			if err := confirmAction(cmd, op); err != nil {
				return err
			}
		}

		parsedQuery, err := ParseQueryPairs(flags.Query)
		if err != nil {
			return hints.Wrap(op, err)
		}

		genericQueryKeys := make(map[string]struct{}, len(parsedQuery))
		queryPairs := make([]executor.QueryPair, 0, len(parsedQuery)+len(queryBindings))
		for _, pair := range parsedQuery {
			queryPairs = append(queryPairs, executor.QueryPair{Key: pair.Key, Value: pair.Value})
			genericQueryKeys[pair.Key] = struct{}{}
		}

		pathArgs, err := collectPathArgs(cmd, op, pathBindings)
		if err != nil {
			return hints.Wrap(op, err)
		}

		namedQueryPairs, err := collectQueryPairs(cmd, queryBindings, genericQueryKeys)
		if err != nil {
			return hints.Wrap(op, err)
		}
		queryPairs = append(queryPairs, namedQueryPairs...)

		body, err := resolveRequestBody(cmd, op, flags.JSONBody, bodyFlags)
		if err != nil {
			return hints.Wrap(op, err)
		}

		req, err := executor.BuildRequest(cmd.Context(), creds, runtimeSpec, op, pathArgs, queryPairs, body)
		if err != nil {
			return hints.Wrap(op, err)
		}

		if flags.DryRun {
			_, err = fmt.Fprintln(cmd.OutOrStdout(), executor.RenderCurl(req, body))
			return err
		}

		respBody, err := executor.DoWithReauth(client, req, creds)
		if err != nil {
			// api commands are machine-oriented: surface the API error
			// envelope as raw JSON instead of the human-readable rendering.
			var httpErr *executor.HTTPError
			if errors.As(err, &httpErr) {
				return executor.NewJSONError(httpErr)
			}
			return err
		}

		return executor.WriteSuccessResponse(cmd.OutOrStdout(), respBody)
	}

	return cmd
}

func registerPathParameterFlags(cmd *cobra.Command, op *spec.Operation, used map[string]struct{}) []parameterBinding {
	pathByName := make(map[string]spec.Parameter, len(op.PathParams))
	for _, param := range op.PathParams {
		pathByName[param.Name] = param
	}

	bindings := make([]parameterBinding, 0, len(op.PathParamOrder))
	for _, name := range op.PathParamOrder {
		param := pathByName[name]
		if param.Name == "" {
			param = spec.Parameter{Name: name, Location: "path", Required: true, Type: "string"}
		}
		binding := createParameterBinding(param, "path", used)
		cmd.Flags().String(binding.FlagName, "", parameterFlagUsage(binding.Param))
		bindings = append(bindings, binding)
	}
	return bindings
}

func registerQueryParameterFlags(cmd *cobra.Command, op *spec.Operation, used map[string]struct{}) []parameterBinding {
	bindings := make([]parameterBinding, 0, len(op.QueryParams))
	for _, param := range op.QueryParams {
		binding := createParameterBinding(param, "query", used)
		cmd.Flags().String(binding.FlagName, "", parameterFlagUsage(binding.Param))
		bindings = append(bindings, binding)
	}
	return bindings
}

func registerBodyFlags(cmd *cobra.Command, op *spec.Operation, used map[string]struct{}) bodyBinding {
	if op.RequestBody == nil {
		return bodyBinding{}
	}

	binding := bodyBinding{SchemaType: normalizedType(op.RequestBody.SchemaType)}

	if binding.SchemaType == "object" && len(op.RequestBody.Fields) > 0 {
		binding.Fields = make([]bodyFieldBinding, 0, len(op.RequestBody.Fields))
		for _, field := range op.RequestBody.Fields {
			flagName := normalizeToken(field.Name)
			if flagName == "" {
				flagName = "body-field"
			}
			jsonLike := isJSONType(field.Type)
			if jsonLike {
				flagName += "-json"
			}
			flagName = reserveFlagName(used, flagName, "body")
			cmd.Flags().String(flagName, "", bodyFieldFlagUsage(field))
			binding.Fields = append(binding.Fields, bodyFieldBinding{
				Field:    field,
				FlagName: flagName,
				JSON:     jsonLike,
			})
		}
		return binding
	}

	jsonLike := isJSONType(binding.SchemaType)
	flagName := "body"
	if jsonLike {
		flagName = "body-json"
	}
	flagName = reserveFlagName(used, flagName, "body")
	cmd.Flags().String(flagName, "", bodyFlagUsage(binding.SchemaType))
	binding.BodyFlag = flagName
	binding.BodyJSON = jsonLike
	return binding
}

func collectPathArgs(cmd *cobra.Command, op *spec.Operation, bindings []parameterBinding) ([]string, error) {
	if len(op.PathParamOrder) == 0 {
		return nil, nil
	}

	args := make([]string, 0, len(op.PathParamOrder))
	for _, binding := range bindings {
		value, set, err := readFlagValue(cmd, binding.FlagName, binding.Param.Type, binding.JSON)
		if err != nil {
			return nil, fmt.Errorf("invalid --%s: %w", binding.FlagName, err)
		}
		if !set {
			return nil, fmt.Errorf("missing required path parameter %q (--%s)", binding.Param.Name, binding.FlagName)
		}
		args = append(args, value)
	}
	return args, nil
}

func collectQueryPairs(cmd *cobra.Command, bindings []parameterBinding, genericKeys map[string]struct{}) ([]executor.QueryPair, error) {
	pairs := make([]executor.QueryPair, 0, len(bindings))
	for _, binding := range bindings {
		value, set, err := readFlagValue(cmd, binding.FlagName, binding.Param.Type, binding.JSON)
		if err != nil {
			return nil, fmt.Errorf("invalid --%s: %w", binding.FlagName, err)
		}

		if !set {
			if binding.Param.Required {
				if _, present := genericKeys[binding.Param.Name]; !present {
					return nil, fmt.Errorf("missing required query parameter %q (--%s or -q %s=value)", binding.Param.Name, binding.FlagName, binding.Param.Name)
				}
			}
			continue
		}

		pairs = append(pairs, executor.QueryPair{
			Key:   binding.Param.Name,
			Value: value,
		})
	}
	return pairs, nil
}

func resolveRequestBody(cmd *cobra.Command, op *spec.Operation, explicitJSON string, binding bodyBinding) (string, error) {
	if strings.TrimSpace(explicitJSON) != "" {
		return explicitJSON, nil
	}
	if op.RequestBody == nil {
		return "", nil
	}

	if len(binding.Fields) > 0 {
		body := "{}"
		setCount := 0
		missing := make([]string, 0)

		for _, field := range binding.Fields {
			if !cmd.Flags().Changed(field.FlagName) {
				if field.Field.Required {
					missing = append(missing, field.Field.Name)
				}
				continue
			}

			raw, err := cmd.Flags().GetString(field.FlagName)
			if err != nil {
				return "", err
			}

			if field.JSON {
				normalized, err := validateAndNormalizeJSON(raw, field.Field.Type)
				if err != nil {
					return "", fmt.Errorf("invalid --%s: %w", field.FlagName, err)
				}
				body, _ = sjson.SetRaw(body, field.Field.Name, normalized)
			} else {
				typed, _, err := parseScalarValue(raw, field.Field.Type)
				if err != nil {
					return "", fmt.Errorf("invalid --%s: %w", field.FlagName, err)
				}
				body, _ = sjson.Set(body, field.Field.Name, typed)
			}
			setCount++
		}

		if len(missing) > 0 {
			return "", fmt.Errorf("missing required body fields: %s", strings.Join(missing, ", "))
		}
		if setCount == 0 {
			if op.RequestBody.Required {
				return "", fmt.Errorf("request body is required")
			}
			return "", nil
		}
		return body, nil
	}

	if binding.BodyFlag == "" {
		if op.RequestBody.Required {
			return "", fmt.Errorf("request body is required")
		}
		return "", nil
	}
	if !cmd.Flags().Changed(binding.BodyFlag) {
		if op.RequestBody.Required {
			return "", fmt.Errorf("request body is required (--%s or --json)", binding.BodyFlag)
		}
		return "", nil
	}

	raw, err := cmd.Flags().GetString(binding.BodyFlag)
	if err != nil {
		return "", err
	}

	if binding.BodyJSON {
		return validateAndNormalizeJSON(raw, binding.SchemaType)
	}

	typed, _, err := parseScalarValue(raw, binding.SchemaType)
	if err != nil {
		return "", fmt.Errorf("invalid --%s: %w", binding.BodyFlag, err)
	}
	encoded, err := json.Marshal(typed)
	if err != nil {
		return "", fmt.Errorf("encode request body: %w", err)
	}
	return string(encoded), nil
}

func readFlagValue(cmd *cobra.Command, flagName, kind string, jsonLike bool) (string, bool, error) {
	if !cmd.Flags().Changed(flagName) {
		return "", false, nil
	}

	raw, err := cmd.Flags().GetString(flagName)
	if err != nil {
		return "", false, err
	}

	if jsonLike {
		normalized, err := validateAndNormalizeJSON(raw, kind)
		if err != nil {
			return "", false, err
		}
		return normalized, true, nil
	}

	_, canonical, err := parseScalarValue(raw, kind)
	if err != nil {
		return "", false, err
	}
	return canonical, true, nil
}

func parseScalarValue(raw, kind string) (any, string, error) {
	switch normalizedType(kind) {
	case "integer":
		value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return nil, "", fmt.Errorf("expected integer")
		}
		return value, strconv.FormatInt(value, 10), nil
	case "number":
		value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return nil, "", fmt.Errorf("expected number")
		}
		return value, strconv.FormatFloat(value, 'f', -1, 64), nil
	case "boolean":
		value, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return nil, "", fmt.Errorf("expected boolean (true|false)")
		}
		return value, strconv.FormatBool(value), nil
	default:
		return raw, raw, nil
	}
}

func validateAndNormalizeJSON(raw, kind string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("expected JSON")
	}
	if !json.Valid([]byte(trimmed)) {
		return "", fmt.Errorf("expected valid JSON")
	}

	switch normalizedType(kind) {
	case "object":
		if !strings.HasPrefix(trimmed, "{") {
			return "", fmt.Errorf("expected JSON object")
		}
	case "array":
		if !strings.HasPrefix(trimmed, "[") {
			return "", fmt.Errorf("expected JSON array")
		}
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(trimmed)); err != nil {
		return "", fmt.Errorf("expected valid JSON")
	}
	return compact.String(), nil
}

func createParameterBinding(param spec.Parameter, prefix string, used map[string]struct{}) parameterBinding {
	flagName := normalizeToken(param.Name)
	if flagName == "" {
		flagName = prefix
	}
	jsonLike := isJSONType(param.Type)
	if jsonLike {
		flagName += "-json"
	}

	flagName = reserveFlagName(used, flagName, prefix)
	return parameterBinding{
		Param:    param,
		FlagName: flagName,
		JSON:     jsonLike,
	}
}

func reserveFlagName(used map[string]struct{}, preferred, prefix string) string {
	candidate := strings.Trim(preferred, "- ")
	if candidate == "" {
		candidate = prefix
	}
	if _, exists := used[candidate]; !exists {
		used[candidate] = struct{}{}
		return candidate
	}

	base := candidate
	if prefix != "" && !strings.HasPrefix(base, prefix+"-") {
		base = prefix + "-" + candidate
	}
	candidate = base
	suffix := 2
	for {
		if _, exists := used[candidate]; !exists {
			used[candidate] = struct{}{}
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, suffix)
		suffix++
	}
}

func buildOperationHelp(op *spec.Operation, pathBindings, queryBindings []parameterBinding, bodyFlags bodyBinding) string {
	lines := []string{}
	if desc := strings.TrimSpace(op.Description); desc != "" {
		lines = append(lines, desc, "")
	}
	lines = append(lines,
		fmt.Sprintf("Operation: %s %s", op.Method, op.Path),
		"Use named flags for operation inputs. Object and array values must be JSON strings.",
	)

	if len(pathBindings) > 0 {
		lines = append(lines, "Path flags:")
		for _, binding := range pathBindings {
			lines = append(lines, fmt.Sprintf("  --%s (%s)%s", binding.FlagName, normalizedType(binding.Param.Type), requiredTag(binding.Param.Required)))
		}
	}
	if len(queryBindings) > 0 {
		lines = append(lines, "Query flags:")
		for _, binding := range queryBindings {
			lines = append(lines, fmt.Sprintf("  --%s (%s)%s", binding.FlagName, normalizedType(binding.Param.Type), requiredTag(binding.Param.Required)))
		}
		lines = append(lines, "  -q key=value may also be used for query params.")
	}

	if op.RequestBody != nil {
		lines = append(lines, "Body inputs:")
		if len(bodyFlags.Fields) > 0 {
			for _, field := range bodyFlags.Fields {
				lines = append(lines, fmt.Sprintf("  --%s (%s)%s", field.FlagName, normalizedType(field.Field.Type), requiredTag(field.Field.Required)))
			}
		} else if bodyFlags.BodyFlag != "" {
			lines = append(lines, fmt.Sprintf("  --%s (%s)%s", bodyFlags.BodyFlag, normalizedType(bodyFlags.SchemaType), requiredTag(op.RequestBody.Required)))
		}
		lines = append(lines, fmt.Sprintf("  --json '%s' overrides body field flags.", hints.BuildBodyTemplate(op)))
	}

	return strings.Join(lines, "\n")
}

func parameterFlagUsage(param spec.Parameter) string {
	required := "optional"
	if param.Required {
		required = "required"
	}
	return fmt.Sprintf("%s %s (%s, sample: %s)", strings.ToUpper(param.Location), required, normalizedType(param.Type), sampleForType(param.Type))
}

func bodyFieldFlagUsage(field spec.BodyField) string {
	required := "optional"
	if field.Required {
		required = "required"
	}
	return fmt.Sprintf("request body field %s (%s, sample: %s)", required, normalizedType(field.Type), sampleForType(field.Type))
}

func bodyFlagUsage(schemaType string) string {
	return fmt.Sprintf("request body (%s, sample: %s)", normalizedType(schemaType), sampleForType(schemaType))
}

func sampleForType(kind string) string {
	switch normalizedType(kind) {
	case "integer", "number":
		return "0"
	case "boolean":
		return "true"
	case "array":
		return "[]"
	case "object":
		return "{}"
	default:
		return `"string"`
	}
}

func requiredTag(required bool) string {
	if required {
		return ", required"
	}
	return ""
}

func normalizedType(kind string) string {
	trimmed := strings.TrimSpace(strings.ToLower(kind))
	if trimmed == "" {
		return "string"
	}
	return trimmed
}

func isJSONType(kind string) bool {
	switch normalizedType(kind) {
	case "object", "array":
		return true
	default:
		return false
	}
}

func copyFlagSet(values map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for key := range values {
		out[key] = struct{}{}
	}
	return out
}

func ensureResourceHierarchy(root *cobra.Command, cache map[string]*cobra.Command, segments []string) *cobra.Command {
	current := root
	keyParts := make([]string, 0, len(segments))
	for _, segment := range segments {
		keyParts = append(keyParts, segment)
		key := strings.Join(keyParts, "/")
		child, exists := cache[key]
		if !exists {
			segmentName := segment
			child = &cobra.Command{
				Use:           segmentName,
				SilenceUsage:  true,
				SilenceErrors: true,
				RunE: func(cmd *cobra.Command, args []string) error {
					return cmd.Help()
				},
			}
			cache[key] = child
			current.AddCommand(child)
		}
		current = child
	}
	return current
}

func uniqueVerbName(parent *cobra.Command, preferred string) string {
	if preferred == "" {
		preferred = "call"
	}
	name := preferred
	if parent.CommandPath() == name {
		name = preferred + "-1"
	}

	attempt := 1
	for hasChildNamed(parent, name) {
		attempt++
		name = fmt.Sprintf("%s-%d", preferred, attempt)
	}
	return name
}

func hasChildNamed(parent *cobra.Command, name string) bool {
	for _, child := range parent.Commands() {
		if child.Name() == name {
			return true
		}
	}
	return false
}

func findOperationContext(operationByCommand map[*cobra.Command]*spec.Operation, cmd *cobra.Command) *spec.Operation {
	for current := cmd; current != nil; current = current.Parent() {
		if op, ok := operationByCommand[current]; ok {
			return op
		}
	}
	return nil
}

func buildOperationShort(op *spec.Operation) string {
	if op == nil {
		return "operation"
	}
	if summary := strings.TrimSpace(op.Summary); summary != "" {
		return summary
	}
	return fmt.Sprintf("%s operation", strings.ToUpper(op.Method))
}

func isWriteMethod(method string) bool {
	switch strings.ToUpper(method) {
	case "POST", "PUT", "DELETE", "PATCH":
		return true
	}
	return false
}

func confirmAction(cmd *cobra.Command, op *spec.Operation) error {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s %s\nProceed? [y/N]: ", strings.ToUpper(op.Method), op.Path)
	scanner := bufio.NewScanner(cmd.InOrStdin())
	scanner.Scan()
	answer := strings.TrimSpace(scanner.Text())
	if strings.ToLower(answer) != "y" {
		return fmt.Errorf("aborted")
	}
	return nil
}
