// CogOS kernel — continuous process daemon for AI agents.
//
// Usage:
//
//	cogos serve [--workspace PATH] [--port PORT]
//	cogos start [--workspace PATH] [--port PORT]
//	cogos stop  [--workspace PATH]
//	cogos status [--workspace PATH]
//	cogos health [--workspace PATH]
//	cogos version
package main

import "github.com/cogos-dev/cogos/internal/engine"

func main() {
	engine.Main()
}
