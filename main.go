package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var (
	s3Client *s3.Client
	bucket   = "your-s3-bucket-name" // Replace with your bucket name
)

func main() {
	// Load AWS configuration
	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		log.Fatalf("AWS config error: %v", err)
	}
	s3Client = s3.NewFromConfig(cfg)

	// Serve static files
	http.Handle("/", http.FileServer(http.Dir("./static")))
	http.HandleFunc("/videos", videoListHandler)
	http.HandleFunc("/video/", streamVideoHandler)

	fmt.Println("📡 GoStream running at http://localhost:8888")
	log.Fatal(http.ListenAndServe(":8888", nil))
}

// Lists available videos in S3
func videoListHandler(w http.ResponseWriter, r *http.Request) {
	resp, err := s3Client.ListObjectsV2(context.TODO(), &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Could not list videos: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("["))
	for i, item := range resp.Contents {
		name := *item.Key
		if strings.HasSuffix(strings.ToLower(name), ".mp4") || strings.HasSuffix(strings.ToLower(name), ".webm") {
			if i > 0 {
				w.Write([]byte(","))
			}
			url := "/video/" + name
			w.Write([]byte(fmt.Sprintf(`{"name":"%s","url":"%s"}`, name, url)))
		}
	}
	w.Write([]byte("]"))
}

// Streams video file from S3 with range request support
func streamVideoHandler(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/video/")
	if key == "" {
		http.Error(w, "Video key is missing", http.StatusBadRequest)
		return
	}

	// Get object metadata
	head, err := s3Client.HeadObject(context.TODO(), &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		log.Printf("HeadObject error for %s: %v", key, err)
		http.Error(w, fmt.Sprintf("File not found: %v", err), http.StatusNotFound)
		return
	}

	// Determine Content-Type based on file extension
	contentType := "video/mp4"
	if strings.HasSuffix(strings.ToLower(key), ".webm") {
		contentType = "video/webm"
	}
	log.Printf("Serving %s, Content-Type: %s, Content-Length: %d", key, contentType, *head.ContentLength)

	// Handle range requests
	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" {
		// No range request, serve the entire file
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", *head.ContentLength))
		w.Header().Set("Content-Disposition", "inline")
		w.Header().Set("Accept-Ranges", "bytes")

		out, err := s3Client.GetObject(context.TODO(), &s3.GetObjectInput{
			Bucket: aws.String(bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			log.Printf("GetObject error for %s: %v", key, err)
			http.Error(w, fmt.Sprintf("Error retrieving file: %v", err), http.StatusInternalServerError)
			return
		}
		defer out.Body.Close()

		// Check for small response to log potential errors
		if *head.ContentLength < 1024 {
			data, err := io.ReadAll(out.Body)
			if err != nil {
				log.Printf("Error reading body for %s: %v", key, err)
			} else {
				log.Printf("Small response body for %s: %s", key, string(data))
			}
			http.Error(w, "Invalid video file", http.StatusBadRequest)
			return
		}

		// Stream the file to the client
		_, err = io.Copy(w, out.Body)
		if err != nil {
			log.Printf("io.Copy error for %s: %v", key, err)
		}
		return
	}

	// Handle partial content (range request)
	ranges := strings.Split(strings.TrimPrefix(rangeHeader, "bytes="), "-")
	start, err := parseRange(ranges[0])
	if err != nil {
		http.Error(w, "Invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	end := *head.ContentLength - 1
	if ranges[1] != "" {
		end, err = parseRange(ranges[1])
		if err != nil || end >= *head.ContentLength {
			http.Error(w, "Invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
	}

	// Ensure start is less than end and within file bounds
	if start > end || start >= *head.ContentLength {
		http.Error(w, "Invalid range", http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// Get the partial object
	out, err := s3Client.GetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", start, end)),
	})
	if err != nil {
		log.Printf("GetObject error for %s with range %s: %v", key, rangeHeader, err)
		http.Error(w, fmt.Sprintf("Error retrieving file range: %v", err), http.StatusInternalServerError)
		return
	}
	defer out.Body.Close()

	// Set headers for partial content
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, *head.ContentLength))
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusPartialContent)

	// Stream the range to the client
	_, err = io.Copy(w, out.Body)
	if err != nil {
		log.Printf("io.Copy error for %s with range %s: %v", key, rangeHeader, err)
	}
}

// Helper function to parse range values
func parseRange(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}
