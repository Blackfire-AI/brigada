package logo

import (
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/mattn/go-isatty"
	"golang.org/x/sys/unix"
)

// Detect fills Caps for output written to w, from the environment and,
// when w is a terminal, the terminal's size. Output that is not an
// *os.File on a terminal gets no color.
func Detect(w io.Writer) Caps {
	f, ok := w.(*os.File)
	tty := ok && isatty.IsTerminal(f.Fd())
	width := 0
	if tty {
		if ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ); err == nil {
			width = int(ws.Col)
		}
	}
	return fromEnv(os.Getenv, tty, width)
}

// fromEnv is Detect without the terminal: getenv reads the environment,
// tty says whether the output is a terminal and width is its size in
// columns (0 when unknown, then COLUMNS is used).
func fromEnv(getenv func(string) string, tty bool, width int) Caps {
	term := getenv("TERM")
	caps := Caps{TTY: tty, Width: width, UTF8: term != "dumb" && utf8Locale(getenv)}
	if caps.Width <= 0 {
		caps.Width, _ = strconv.Atoi(getenv("COLUMNS"))
		caps.Width = max(caps.Width, 0)
	}
	switch colorterm := getenv("COLORTERM"); {
	case !tty || getenv("NO_COLOR") != "" || term == "dumb":
		caps.Color = NoColor
	case colorterm == "truecolor" || colorterm == "24bit":
		caps.Color = TrueColor
	case strings.Contains(term, "256color"):
		caps.Color = Color256
	default:
		caps.Color = Color16
	}
	return caps
}

// utf8Locale reports whether the locale's character set is UTF-8: the
// first of LC_ALL, LC_CTYPE and LANG that is set decides, and no locale
// at all is the C locale, which is not.
func utf8Locale(getenv func(string) string) bool {
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := getenv(name); v != "" {
			v = strings.ToLower(v)
			return strings.Contains(v, "utf-8") || strings.Contains(v, "utf8")
		}
	}
	return false
}
