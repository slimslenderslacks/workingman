package task

import "os"

func writeRaw(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
