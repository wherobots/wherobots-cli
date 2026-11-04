package commands

import (
	"net/http"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// httptest.Server.Close closes the shared transport's idle connections, which breaks a parallel write.
	http.DefaultTransport.(*http.Transport).DisableKeepAlives = true
	os.Exit(m.Run())
}
