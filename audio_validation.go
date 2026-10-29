package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
)

var errAudioValidatorUnavailable = errors.New("audio validator is unavailable")

type invalidAudioError struct {
	message string
}

func (err invalidAudioError) Error() string { return err.message }

func writeAudioValidationError(w http.ResponseWriter, r *http.Request, err error, operation string) bool {
	var invalid invalidAudioError
	if errors.As(err, &invalid) {
		http.Error(w, invalid.Error(), http.StatusBadRequest)
		return true
	}
	if errors.Is(err, errAudioValidatorUnavailable) {
		captureSentryError(r.Context(), err, "media_validation", operation)
		http.Error(w, "audio validation is temporarily unavailable; retry later", http.StatusServiceUnavailable)
		return true
	}
	return false
}

func audioFFmpegBinary() string {
	if binary := strings.TrimSpace(os.Getenv("FFMPEG_BINARY")); binary != "" {
		return binary
	}
	return "ffmpeg"
}

// Validate before creating a publicly accessible file or registering an upload.
// Multipart files may be memory-backed, so FFmpeg needs a private disk copy.
func validateUploadedMP3(ctx context.Context, source multipart.File) error {
	staged, err := os.CreateTemp("", "esketit-audio-validation-*.mp3")
	if err != nil {
		return fmt.Errorf("create audio validation file: %w", err)
	}
	defer func() {
		if err := removeFileForCleanup(staged.Name()); err != nil {
			captureSentryError(ctx, fmt.Errorf("remove audio validation file: %w", err), "storage", "upload.audio.cleanup")
		}
	}()
	size, copyErr := io.Copy(staged, source)
	if err := errors.Join(copyErr, staged.Close()); err != nil {
		return fmt.Errorf("stage audio for validation: %w", err)
	}
	if size == 0 {
		return invalidAudioError{message: "uploaded file is empty"}
	}
	if err := validateMP3Audio(ctx, staged.Name()); err != nil {
		return err
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind validated audio: %w", err)
	}
	return nil
}

func mp3UploadFileName(originalName string) string {
	base := strings.TrimSuffix(originalName, filepath.Ext(originalName))
	if base == "" {
		base = "audio"
	}
	return base + ".mp3"
}

// Check the actual MP3 stream and decode it completely. A filename, MIME header,
// or ID3 tag alone does not establish that a file contains playable audio.
func validateMP3Audio(ctx context.Context, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open audio for validation: %w", err)
	}
	defer file.Close()
	probe := make([]byte, 64<<10)
	size, err := file.Read(probe)
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read audio for validation: %w", err)
	}
	probe = probe[:size]
	if len(probe) >= 10 && string(probe[:3]) == "ID3" {
		for _, value := range probe[6:10] {
			if value&0x80 != 0 {
				return invalidAudioError{message: "invalid MP3 ID3 header"}
			}
		}
		offset := 10 + (int(probe[6]) << 21) + (int(probe[7]) << 14) + (int(probe[8]) << 7) + int(probe[9])
		if probe[5]&0x10 != 0 {
			offset += 10
		}
		if _, err := file.Seek(int64(offset), io.SeekStart); err != nil {
			return invalidAudioError{message: "invalid MP3 header"}
		}
		size, err = file.Read(probe[:cap(probe)])
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read MP3 frames: %w", err)
		}
		probe = probe[:size]
	}
	isMP3 := false
	for index := 0; index+4 <= len(probe); index++ {
		first, second, third := probe[index], probe[index+1], probe[index+2]
		if first == 0xff && second&0xe0 == 0xe0 && second&0x18 != 0x08 && second&0x06 == 0x02 && third&0xf0 != 0 && third&0xf0 != 0xf0 && third&0x0c != 0x0c {
			isMP3 = true
			break
		}
	}
	if !isMP3 {
		return invalidAudioError{message: "file must contain MP3 audio"}
	}
	decodeContext, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(decodeContext, audioFFmpegBinary(), "-nostdin", "-v", "error", "-xerror", "-err_detect", "explode", "-f", "mp3", "-i", path, "-map", "0:a:0", "-f", "null", "-")
	if err := command.Run(); err != nil {
		if decodeContext.Err() != nil {
			return fmt.Errorf("%w: %w", errAudioValidatorUnavailable, decodeContext.Err())
		}
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			return fmt.Errorf("%w: %w", errAudioValidatorUnavailable, err)
		}
		return invalidAudioError{message: "MP3 audio could not be decoded completely"}
	}
	return nil
}

// Keep valid MP3 bytes intact. Other supported audio is converted privately and
// replaces the download only after the complete output passes validation.
func normalizeImportedMP3(ctx context.Context, path string) error {
	validationContext, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	err := validateMP3Audio(validationContext, path)
	if err == nil {
		return nil
	}
	var invalid invalidAudioError
	if !errors.As(err, &invalid) {
		return err
	}
	output, err := os.CreateTemp(filepath.Dir(path), "normalized-audio-*.mp3")
	if err != nil {
		return fmt.Errorf("create normalized audio: %w", err)
	}
	defer func() {
		if err := removeFileForCleanup(output.Name()); err != nil {
			captureSentryError(ctx, fmt.Errorf("remove normalized audio: %w", err), "storage", "import.audio.cleanup")
		}
	}()
	if err := output.Close(); err != nil {
		return fmt.Errorf("close normalized audio: %w", err)
	}
	command := exec.CommandContext(validationContext, audioFFmpegBinary(),
		"-nostdin", "-y", "-v", "error", "-xerror", "-err_detect", "explode",
		"-protocol_whitelist", "file,pipe",
		"-format_whitelist", "mp3,mov,mp4,m4a,3gp,3g2,mj2,ogg,flac,wav,aac,aiff,asf,matroska,webm",
		"-i", path, "-map", "0:a:0", "-vn", "-codec:a", "libmp3lame", "-q:a", "2",
		"-fs", strconv.FormatInt(maxSongUploadSize+1, 10), "-f", "mp3", output.Name())
	if err := command.Run(); err != nil {
		if validationContext.Err() != nil {
			return fmt.Errorf("%w: %w", errAudioValidatorUnavailable, validationContext.Err())
		}
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			return fmt.Errorf("%w: %w", errAudioValidatorUnavailable, err)
		}
		return invalidAudioError{message: "audio could not be decoded and converted to MP3"}
	}
	info, err := os.Stat(output.Name())
	if err != nil {
		return fmt.Errorf("inspect normalized audio: %w", err)
	}
	if info.Size() >= maxSongUploadSize {
		return invalidAudioError{message: "converted MP3 exceeds the upload size limit"}
	}
	if err := validateMP3Audio(validationContext, output.Name()); err != nil {
		return err
	}
	if err := os.Rename(output.Name(), path); err != nil {
		return fmt.Errorf("replace normalized audio: %w", err)
	}
	if hub := sentry.GetHubFromContext(ctx); hub != nil {
		hub.AddBreadcrumb(&sentry.Breadcrumb{Category: "audio.validation", Message: "Converted imported audio to validated MP3", Level: sentry.LevelInfo}, nil)
	}
	return nil
}
