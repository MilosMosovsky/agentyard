//go:build !darwin

package main

import "os"

// isTerminal is only asked by install and uninstall, which are macOS only;
// elsewhere there is never a terminal to ask in.
func isTerminal(*os.File) bool { return false }
