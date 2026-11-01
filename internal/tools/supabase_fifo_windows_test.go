//go:build windows

package tools

import "errors"

func syscallMkfifo(string) error { return errors.New("no fifos on windows") }
