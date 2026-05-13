package project

import "os"

func writeEmpty(path string) error {
	return os.WriteFile(path, nil, 0o644)
}
