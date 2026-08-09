package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

func confirm(in *bufio.Reader, out io.Writer, prompt string) bool {
	if _, err := fmt.Fprintf(out, "%s[y/N] ", prompt); err != nil {
		return false
	}
	line, _ := in.ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "y")
}
