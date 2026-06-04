// migrate_api_prefix rewrites tracks_db.json so that audioFilePath and
// coverImagePath values served by this backend pick up the new /api/ route
// prefix. It is idempotent (already-prefixed values are left alone) and
// preserves external absolute URLs untouched.
//
// Usage:
//
//	go run ./cmd/migrate_api_prefix              # operates on ./tracks_db.json
//	go run ./cmd/migrate_api_prefix path/to.json
//	go run ./cmd/migrate_api_prefix -n           # dry run, prints diff summary
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"time"
)

func main() {
	dryRun := flag.Bool("n", false, "dry run: show what would change, do not write")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [-n] [path-to-tracks_db.json]\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	path := "tracks_db.json"
	if flag.NArg() > 0 {
		path = flag.Arg(0)
	}

	original, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s: %v", path, err)
	}

	rewritten, stats := migrate(original)

	log.Printf("audioFilePath: %d migrated, %d already had /api prefix, %d skipped (absolute URL or unknown form)",
		stats.audioMigrated, stats.audioAlreadyPrefixed, stats.audioSkipped)
	log.Printf("coverImagePath: %d migrated, %d already had /api prefix, %d skipped (absolute URL or unknown form)",
		stats.coverMigrated, stats.coverAlreadyPrefixed, stats.coverSkipped)

	if stats.audioMigrated == 0 && stats.coverMigrated == 0 {
		log.Printf("nothing to migrate in %s", path)
		return
	}

	if *dryRun {
		log.Printf("dry run: %s would be modified", path)
		return
	}

	backup := fmt.Sprintf("%s.bak.%s", path, time.Now().UTC().Format("20060102T150405Z"))
	if err := os.WriteFile(backup, original, 0o644); err != nil {
		log.Fatalf("write backup %s: %v", backup, err)
	}
	log.Printf("backup written to %s", backup)

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, rewritten, 0o644); err != nil {
		log.Fatalf("write %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Fatalf("rename %s -> %s: %v", tmp, path, err)
	}
	log.Printf("%s migrated in place", path)
}

type migrationStats struct {
	audioMigrated, audioAlreadyPrefixed, audioSkipped int
	coverMigrated, coverAlreadyPrefixed, coverSkipped int
}

var (
	audioFieldRE = regexp.MustCompile(`("audioFilePath"\s*:\s*)"([^"\\]*(?:\\.[^"\\]*)*)"`)
	coverFieldRE = regexp.MustCompile(`("coverImagePath"\s*:\s*)"([^"\\]*(?:\\.[^"\\]*)*)"`)
)

func migrate(input []byte) ([]byte, migrationStats) {
	stats := migrationStats{}

	output := audioFieldRE.ReplaceAllFunc(input, func(match []byte) []byte {
		groups := audioFieldRE.FindSubmatch(match)
		prefix, value := groups[1], string(groups[2])
		newValue, action := rewritePath(value, "/songs/")
		switch action {
		case actionMigrated:
			stats.audioMigrated++
		case actionAlreadyPrefixed:
			stats.audioAlreadyPrefixed++
		case actionSkipped:
			stats.audioSkipped++
		}
		return []byte(fmt.Sprintf(`%s"%s"`, prefix, newValue))
	})

	output = coverFieldRE.ReplaceAllFunc(output, func(match []byte) []byte {
		groups := coverFieldRE.FindSubmatch(match)
		prefix, value := groups[1], string(groups[2])
		newValue, action := rewritePath(value, "/album-covers/")
		switch action {
		case actionMigrated:
			stats.coverMigrated++
		case actionAlreadyPrefixed:
			stats.coverAlreadyPrefixed++
		case actionSkipped:
			stats.coverSkipped++
		}
		return []byte(fmt.Sprintf(`%s"%s"`, prefix, newValue))
	})

	return output, stats
}

type rewriteAction int

const (
	actionMigrated rewriteAction = iota
	actionAlreadyPrefixed
	actionSkipped
)

// rewritePath returns the value to write and an action label.
// It prefixes /api before the given oldPrefix when the value is a
// server-local path. Empty strings, values with a scheme (http://, https://),
// and values already prefixed with /api are left alone.
func rewritePath(value, oldPrefix string) (string, rewriteAction) {
	if value == "" {
		return value, actionSkipped
	}
	if hasScheme(value) {
		return value, actionSkipped
	}
	newPrefix := "/api" + oldPrefix
	if startsWith(value, newPrefix) {
		return value, actionAlreadyPrefixed
	}
	if startsWith(value, oldPrefix) {
		return "/api" + value, actionMigrated
	}
	return value, actionSkipped
}

func hasScheme(s string) bool {
	// minimal scheme detection: <letter>+[a-z0-9+-.]*://
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 0 {
			if !isAlpha(c) {
				return false
			}
			continue
		}
		if c == ':' {
			return i+2 < len(s) && s[i+1] == '/' && s[i+2] == '/'
		}
		if !(isAlpha(c) || isDigit(c) || c == '+' || c == '-' || c == '.') {
			return false
		}
	}
	return false
}

func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
