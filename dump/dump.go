package dump

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/gunk/gunk/generate"
	"github.com/gunk/gunk/protoutil"
)

// Run will generate the FileDescriptorSet for a Gunk package, and
// output it as required.
func Run(format, dir string, patterns ...string) error {
	// Load the Gunk package and generate the FileDescriptorSet for the
	// Gunk package.
	fds, err := generate.FileDescriptorSet(dir, patterns...)
	if err != nil {
		return err
	}

	// Format the FileDescriptorSet.
	var bs []byte
	switch format {
	case "json":
		bs, err = json.Marshal(fds)
		if err != nil {
			return err
		}
	case "", "proto":
		// The default format.
		bs, err = protoutil.MarshalDeterministic(fds)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown output format %q", format)
	}

	// Otherwise, output to stdout
	_, err = os.Stdout.Write(bs)
	return err
}
