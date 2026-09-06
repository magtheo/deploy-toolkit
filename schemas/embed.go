package schemas

import (
	"embed"
	"fmt"
)

//go:embed *.schema.json
var files embed.FS

func Raw() (map[string][]byte, error) {
	entries, err := files.ReadDir(".")
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := files.ReadFile(e.Name())
		if err != nil {
			return nil, err
		}
		out[e.Name()] = b
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no schemas embedded")
	}
	return out, nil
}
