package orchestrator

import (
    "context"
    "bytes"
    "fmt"
    "os"
    "strings"

    awscfg "github.com/aws/aws-sdk-go-v2/config"
    "github.com/aws/aws-sdk-go-v2/service/s3"
)

// SaveAggregatedTextToS3 uploads the aggregated text to S3 and returns the s3:// URL.
// It derives the output location as: results/{jobID}/extracted_text.txt in the same bucket.
func SaveAggregatedTextToS3(ctx context.Context, originalRef, jobID, text string) (string, error) {
    bucket := os.Getenv("AWS_S3_BUCKET")
    key := fmt.Sprintf("results/%s/extracted_text.txt", jobID)
    // If originalRef is an s3:// URL, prefer its bucket
    if strings.HasPrefix(originalRef, "s3://") {
        path := strings.TrimPrefix(originalRef, "s3://")
        parts := strings.SplitN(path, "/", 2)
        if len(parts) == 2 && parts[0] != "" { bucket = parts[0] }
    }
    if bucket == "" { return "", fmt.Errorf("AWS_S3_BUCKET not set") }

    cfg, err := awscfg.LoadDefaultConfig(ctx)
    if err != nil { return "", err }
    cli := s3.NewFromConfig(cfg)
    _, err = cli.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: &key, Body: bytes.NewReader([]byte(text))})
    if err != nil { return "", err }
    return fmt.Sprintf("s3://%s/%s", bucket, key), nil
}

