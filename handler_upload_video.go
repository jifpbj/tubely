package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const uploadLimit = 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, uploadLimit)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "You don't have permission to upload video for this video ID", nil)
		return
	}

	file, fileHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't get image data from form", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(fileHeader.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type for video", err)
		return
	}

	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Unsupported video media type", nil)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
		return
	}

	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	if _, err := io.Copy(tempFile, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't write to temp file", err)
		return
	}
	tempFile.Seek(0, io.SeekStart)

	slice := make([]byte, 32)
	_, err = rand.Read(slice)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate random ID", err)
		return
	}

	aspect, err := cfg.getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video aspect ratio", err)
		return
	}

	var prefix string
	switch aspect {
	case "16:9":
		prefix = "landscape/"
	case "9:16":
		prefix = "portrait/"
	default:
		prefix = "other/"
	}

	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video for fast start", err)
		return
	}

	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed video file", err)
		return
	}

	defer processedFile.Close()

	pathID := hex.EncodeToString(slice)
	key := prefix + pathID + ".mp4"

	log.Printf("Uploading to bucket=%q, key=%q", cfg.s3Bucket, key)

	_, err = cfg.s3Client.PutObject(
		r.Context(),
		&s3.PutObjectInput{
			Bucket:      aws.String(cfg.s3Bucket),
			Key:         aws.String(key),
			Body:        processedFile,
			ContentType: aws.String(mediaType),
		},
	)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload video to S3", err)
		return
	}

	url := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, key)
	video.VideoURL = &url

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video with URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

// FFProbeOutput aspect ratio helper
type FFProbeOutput struct {
	Streams []struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"streams"`
}

func (cfg *apiConfig) getVideoAspectRatio(filePath string) (string, error) {
	log.Println("ffprobe starting with filepath:", filePath)

	command := exec.Command(
		"ffprobe",
		"-v", "error", "-show_streams", "-of", "json",
		filePath)

	var stdout bytes.Buffer
	command.Stdout = &stdout

	err := command.Run()
	if err != nil {
		return "", err
	}

	var ffprobeOutput FFProbeOutput

	err = json.Unmarshal(stdout.Bytes(), &ffprobeOutput)
	if err != nil {
		log.Println("json unmarshal error:", err)
		return "", err
	}

	if len(ffprobeOutput.Streams) == 0 {
		log.Println("no streams found in ffprobe output")
		return "", fmt.Errorf("no streams found in ffprobe output")
	}

	width := ffprobeOutput.Streams[0].Width
	height := ffprobeOutput.Streams[0].Height

	ratio := float64(width) / float64(height)

	landscape := 16.0 / 9.0
	portrait := 9.0 / 16.0
	tolerance := 0.05

	if math.Abs(ratio-landscape) <= tolerance {
		return "16:9", nil
	} else if math.Abs(ratio-portrait) <= tolerance {
		return "9:16", nil
	} else {
		return "other", nil
	}
}

func processVideoForFastStart(filePath string) (string, error) {
	ext := filepath.Ext(filePath)                // ".mp4"
	base := strings.TrimSuffix(filePath, ext)    // "/tmp/tubely-upload"
	outputFilePath := base + ".processing" + ext // "/tmp/tubely-upload.processing.mp4"
	command := exec.Command(
		"ffmpeg",
		"-i",
		filePath,
		"-c",
		"copy",
		"-movflags",
		"faststart",
		"-f",
		"mp4",
		outputFilePath)

	var stderr bytes.Buffer
	command.Stderr = &stderr

	err := command.Run()
	if err != nil {
		return "", fmt.Errorf("ffmpeg error: %v, %s", err, stderr.String())
	}

	info, err := os.Stat(outputFilePath)
	if err != nil {
		return "", fmt.Errorf("processed file not found: %v", err)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("processed file is empty")
	}

	return outputFilePath, nil
}
