package worker_test

import "os"

// openFile is the test-only helper used by the no-subprocess sentinel
// in client_placeholder_test.go to inspect the placeholder source.
// Kept in its own file so the os import does not bleed across tests.
func openFile(name string) (*os.File, error) {
	return os.Open(name)
}
