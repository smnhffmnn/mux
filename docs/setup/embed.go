package setup

import "embed"

//go:embed *.md
var Docs embed.FS

func Get(typ string) string {
	data, err := Docs.ReadFile(typ + ".md")
	if err != nil {
		return ""
	}
	return string(data)
}
