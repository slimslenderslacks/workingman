package daemon

import "os"

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

func mkdirAll(path string) error {
	return os.MkdirAll(path, 0o755)
}
