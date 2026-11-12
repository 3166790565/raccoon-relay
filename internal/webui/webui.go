package webui

import "embed"

//go:embed index.html app.js style.css
var files embed.FS

// Read 返回内嵌的静态资源。
func Read(name string) ([]byte, error) {
	return files.ReadFile(name)
}
