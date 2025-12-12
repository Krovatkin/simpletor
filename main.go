package main

import (
	"embed"
	"flag"
	"fmt"
	"log"
	"net/http"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"github.com/gofiber/websocket/v2"
	"simpletor/server"
)

//go:embed static/*
var embedFS embed.FS

func main() {
	port := flag.Int("port", 3000, "Port to listen on")
	flag.Parse()

	app := fiber.New(fiber.Config{
		DisableStartupMessage: false,
	})

	// Initialize Multi-LSP manager
	lspManager := server.NewMultiLSPManager()
	defer lspManager.ShutdownAll()

	// WebSocket upgrade middleware
	app.Use("/ws", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			c.Locals("lspManager", lspManager)
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})

	// WebSocket endpoint
	app.Get("/ws", websocket.New(server.HandleWebSocket))

	// Serve static files from embedded FS
	app.Use("/", filesystem.New(filesystem.Config{
		Root:       http.FS(embedFS),
		PathPrefix: "static",
		Browse:     false,
	}))

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("Starting server on %s", addr)
	log.Fatal(app.Listen(addr))
}
