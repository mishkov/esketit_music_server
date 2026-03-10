package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type songInfo struct {
	Name         string    `json:"name"`
	SizeBytes    int64     `json:"sizeBytes"`
	LastModified time.Time `json:"lastModified"`
	URL          string    `json:"url"`
}

func main() {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("cannot resolve home directory: %v", err)
	}

	defaultSongsDir := filepath.Join(home, "Projects", "esketit_music", "media_storage", "songs")
	songsDir := os.Getenv("SONGS_DIR")
	if songsDir == "" {
		songsDir = defaultSongsDir
	}

	if err := ensureDir(songsDir); err != nil {
		log.Fatalf("invalid songs directory %q: %v", songsDir, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /songs", listSongsHandler(songsDir))
	mux.HandleFunc("GET /songs/", getSongHandler(songsDir))

	addr := ":8080"
	log.Printf("server listening on %s", addr)
	log.Printf("serving songs from %s", songsDir)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func ensureDir(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("path is not a directory")
	}
	return nil
}

func listSongsHandler(songsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entries, err := os.ReadDir(songsDir)
		if err != nil {
			http.Error(w, "failed to read songs directory", http.StatusInternalServerError)
			return
		}

		songs := make([]songInfo, 0, len(entries))
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}

			name := entry.Name()
			// fullPath := filepath.Join(songsDir, name)
			info, err := entry.Info()
			if err != nil {
				continue
			}

			songs = append(songs, songInfo{
				Name:         name,
				SizeBytes:    info.Size(),
				LastModified: info.ModTime(),
				URL:          "/songs/" + url.PathEscape(name),
			})
		}

		sort.Slice(songs, func(i, j int) bool {
			return strings.ToLower(songs[i].Name) < strings.ToLower(songs[j].Name)
		})

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(songs); err != nil {
			http.Error(w, "failed to write response", http.StatusInternalServerError)
			return
		}
	}
}

func getSongHandler(songsDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		encodedName := strings.TrimPrefix(r.URL.Path, "/songs/")
		if encodedName == "" {
			http.NotFound(w, r)
			return
		}

		name, err := url.PathUnescape(encodedName)
		if err != nil {
			http.Error(w, "invalid song name", http.StatusBadRequest)
			return
		}

		cleanName := filepath.Base(filepath.Clean(name))
		if cleanName == "." || cleanName == "/" || cleanName == "" {
			http.Error(w, "invalid song name", http.StatusBadRequest)
			return
		}

		fullPath := filepath.Join(songsDir, cleanName)
		if _, err := os.Stat(fullPath); err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "failed to read song", http.StatusInternalServerError)
			return
		}

		http.ServeFile(w, r, fullPath)
	}
}
