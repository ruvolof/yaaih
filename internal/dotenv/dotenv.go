// Package dotenv loads environment variables from a .env file so secrets named
// by `provider.api_key_env` (e.g. OPENROUTER_API_KEY) can live in a file next
// to the binary instead of being exported by hand before every run.
//
// The real environment always wins: a key already set in the process
// environment is never overwritten by the file.
package dotenv

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// Filename is the file the loader looks for in each candidate directory.
const Filename = ".env"

// Load reads the .env file in dir and exports every key it defines that is not
// already present in the environment. A missing file is not an error: it
// returns loaded=false with a nil error. Malformed lines are skipped.
func Load(dir string) (loaded bool, err error) {
	path := filepath.Join(dir, Filename)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := parseLine(sc.Text())
		if !ok {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return true, err
		}
	}
	if err := sc.Err(); err != nil {
		return true, err
	}
	return true, nil
}

// LoadDefault looks for a .env in the working directory and, failing that,
// next to the harness binary — so the harness picks up the project's .env
// whether it is launched from the source tree or by absolute path. It returns
// the path of the file it loaded, or "" if none was found.
func LoadDefault() (string, error) {
	var dirs []string
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, dir := range dirs {
		loaded, err := Load(dir)
		if err != nil {
			return "", err
		}
		if loaded {
			return filepath.Join(dir, Filename), nil
		}
	}
	return "", nil
}

// parseLine splits one .env line into a key/value pair. It accepts an optional
// `export ` prefix, ignores blank lines and `#` comments, strips matching
// single or double quotes, and honours \n, \t, \" and \\ escapes inside
// double-quoted values. Unquoted values lose any trailing ` #` comment.
func parseLine(line string) (key, val string, ok bool) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "#") {
		return "", "", false
	}
	s = strings.TrimPrefix(s, "export ")

	eq := strings.IndexByte(s, '=')
	if eq <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(s[:eq])
	if !validKey(key) {
		return "", "", false
	}
	val = strings.TrimSpace(s[eq+1:])

	switch {
	case len(val) >= 2 && val[0] == '"' && strings.HasSuffix(val, `"`):
		val = unescape(val[1 : len(val)-1])
	case len(val) >= 2 && val[0] == '\'' && strings.HasSuffix(val, `'`):
		val = val[1 : len(val)-1]
	default:
		if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
	}
	return key, val, true
}

// validKey reports whether k looks like a shell environment variable name.
func validKey(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// unescape expands the escape sequences allowed inside a double-quoted value.
func unescape(s string) string {
	r := strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\"`, `"`, `\\`, `\`)
	return r.Replace(s)
}
