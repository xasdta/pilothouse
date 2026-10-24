//go:build !unix

package preview

import "os"

func openNoBlock(p string) (*os.File, error) { return os.Open(p) }
