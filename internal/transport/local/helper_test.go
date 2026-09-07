package local

import "os"

func writeFile(path, content string, mode os.FileMode) error {
	return os.WriteFile(path, []byte(content), mode)
}
