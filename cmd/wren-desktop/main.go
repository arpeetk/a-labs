package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"github.com/summiteight/wren/internal/desktop"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := desktop.New()
	if err := wails.Run(&options.App{
		Title:            "Wren",
		Width:            1440,
		Height:           940,
		MinWidth:         1080,
		MinHeight:        700,
		Frameless:        false,
		BackgroundColour: &options.RGBA{R: 12, G: 14, B: 18, A: 1},
		AssetServer:      &assetserver.Options{Assets: assets},
		OnStartup:        app.Startup,
		OnShutdown:       app.Shutdown,
		Bind:             []any{app},
	}); err != nil {
		log.Fatal(err)
	}
}
