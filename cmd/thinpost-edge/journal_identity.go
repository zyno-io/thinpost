package main

import (
	"fmt"
	"io"

	"github.com/zyno-io/thinpost/internal/journal"
)

// Inspect or initialize an empty journal only while no worker has it open.
func printJournalIdentity(path string, output io.Writer) error {
	j, err := journal.Open(path, 1)
	if err != nil {
		return fmt.Errorf("cannot open edge journal: %w", err)
	}
	defer j.Close()
	identity, err := j.Identity()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, identity)
	return err
}
