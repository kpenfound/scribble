package tailnet

import "os"

func writeFile(path string) error { return os.WriteFile(path, []byte("x"), 0o600) }

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
