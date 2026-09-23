// Package files implements the Wherobots Files operations (list, mkdir,
// upload, download, rename, delete) once, against a drive.
package files

import "fmt"

// Kind names a type of drive. Today there is only the personal my-files
// drive; a shared drive would add a kind here and a case in StorageID.
type Kind string

const (
	// KindMyFiles is the caller's personal files area in one region.
	KindMyFiles Kind = "my-files"
)

// Drive identifies one Files area. Every file operation is written against
// a Drive, so a new kind only has to say how it maps to a storage id.
type Drive struct {
	Kind   Kind
	Region string
}

// MyFiles returns the caller's personal drive in region.
func MyFiles(region string) Drive {
	return Drive{Kind: KindMyFiles, Region: region}
}

// Label is the user-facing drive name used in messages.
func (d Drive) Label() string {
	return string(d.Kind)
}

// StorageID is the {storage_id} path value the /storage routes expect.
func (d Drive) StorageID() (string, error) {
	switch d.Kind {
	case KindMyFiles:
		if d.Region == "" {
			return "", fmt.Errorf("a region is required for %s", d.Label())
		}
		return "user_files::" + d.Region, nil
	default:
		return "", fmt.Errorf("unknown drive kind %q", d.Kind)
	}
}
