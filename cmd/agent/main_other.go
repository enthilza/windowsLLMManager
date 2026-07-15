//go:build !windows

package main

import "fmt"

func main() {
	fmt.Println("agent.exe is supported on Windows only")
}
