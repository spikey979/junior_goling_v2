package storage

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/pbkdf2"
)

// S3Client wraps AWS S3 client with decryption capabilities
type S3Client struct {
	client     *s3.Client
	bucketName string
}

// FileMetadata represents metadata about a stored file
type FileMetadata struct {
	OriginalName     string            `json:"original_name"`
	ContentType      string            `json:"content_type"`
	Size             int64             `json:"size"`
	Encrypted        bool              `json:"encrypted"`
	Metadata         map[string]string `json:"metadata"`
	EncryptionFormat string            `json:"encryption_format,omitempty"` // "GCM3NCR0", "3NCR0PTD", or "legacy_gcm"
}

// NewS3Client creates a new S3 client
func NewS3Client(ctx context.Context, bucketName string) (*S3Client, error) {
	cfg, err := awscfg.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	cli := s3.NewFromConfig(cfg)

	return &S3Client{
		client:     cli,
		bucketName: bucketName,
	}, nil
}

// DownloadFile downloads and decrypts a file from S3
func (s *S3Client) DownloadFile(ctx context.Context, key, password string) ([]byte, *FileMetadata, error) {
	// Download from S3
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to download from S3: %w", err)
	}
	defer result.Body.Close()

	// Read encrypted data
	encryptedData, err := io.ReadAll(result.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read S3 object: %w", err)
	}

	// Extract metadata
	metadata := &FileMetadata{
		Encrypted: true,
		Metadata:  make(map[string]string),
	}

	if result.Metadata != nil {
		// Get the actual filename from Ghost Server's x-amz-meta-name (returned as "Name")
		if name, ok := result.Metadata["name"]; ok {
			metadata.OriginalName = name
			log.Debug().Str("source", "metadata[name]").Str("filename", name).Msg("found original filename from S3 metadata")
		} else if name, ok := result.Metadata["Name"]; ok {
			// Try capitalized version
			metadata.OriginalName = name
			log.Debug().Str("source", "metadata[Name]").Str("filename", name).Msg("found original filename from S3 metadata")
		}

		// Extract ALL custom metadata
		for k, v := range result.Metadata {
			metadata.Metadata[strings.ToLower(k)] = v
		}

		// Check for encryption format in metadata (for files that store it)
		if encFormat, ok := result.Metadata["encryption-format"]; ok {
			metadata.EncryptionFormat = encFormat
		} else if encFormat, ok := result.Metadata["Encryption-Format"]; ok {
			metadata.EncryptionFormat = encFormat
		}
	}

	// Set size
	if result.ContentLength != nil {
		metadata.Size = *result.ContentLength
	}

	// Decrypt the data and detect encryption format
	decryptedData, encryptionFormat, err := s.decryptData(encryptedData, password)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to decrypt data: %w", err)
	}

	// Store the detected encryption format in metadata
	metadata.EncryptionFormat = encryptionFormat
	log.Info().
		Str("key", key).
		Str("encryption_format", encryptionFormat).
		Str("original_name", metadata.OriginalName).
		Int("size", len(decryptedData)).
		Msg("downloaded and decrypted file from S3")

	return decryptedData, metadata, nil
}

// decryptData decrypts data supporting both GCM (new) and CBC legacy (FileAPI) formats
// Returns decrypted data and the detected encryption format
func (s *S3Client) decryptData(encryptedData []byte, password string) ([]byte, string, error) {
	if len(encryptedData) < 8 {
		return nil, "", fmt.Errorf("encrypted data too short: %d bytes", len(encryptedData))
	}

	// Auto-detect format by magic number
	magicNumber := encryptedData[:8]

	switch string(magicNumber) {
	case "GCM3NCR0":
		// New GCM format (hardware-accelerated, more secure)
		log.Debug().Msg("detected GCM format encryption")
		data, err := s.decryptGCM(encryptedData, password)
		return data, "GCM3NCR0", err

	case "3NCR0PTD":
		// FileAPI legacy CBC format (for backward compatibility)
		log.Debug().Msg("detected FileAPI legacy CBC format encryption")
		data, err := s.decryptLegacyCBC(encryptedData, password)
		return data, "3NCR0PTD", err

	default:
		// Try legacy format without magic number for very old files
		log.Debug().Msg("no magic number found, trying legacy GCM fallback")
		data, err := s.decryptLegacyGCM(encryptedData, password)
		return data, "legacy_gcm", err
	}
}

// decryptGCM decrypts data using new GCM format (hardware-accelerated)
func (s *S3Client) decryptGCM(encryptedData []byte, password string) ([]byte, error) {
	// Format: magic(8) + salt(16) + nonce(12) + encrypted_data + auth_tag(16)
	if len(encryptedData) < 8+16+12+16 {
		return nil, fmt.Errorf("GCM data too short: %d bytes", len(encryptedData))
	}

	// Extract components
	salt := encryptedData[8:24]
	nonce := encryptedData[24:36]
	encryptedWithTag := encryptedData[36:]

	// Derive key using PBKDF2
	key := pbkdf2.Key([]byte(password), salt, 100000, 32, sha256.New)

	// Create AES cipher
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	// Create GCM mode (hardware-accelerated on modern CPUs)
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	// Decrypt data
	plaintext, err := gcm.Open(nil, nonce, encryptedWithTag, nil)
	if err != nil {
		return nil, fmt.Errorf("GCM decryption failed: %w", err)
	}

	log.Debug().Msg("successfully decrypted using hardware-accelerated GCM")
	return plaintext, nil
}

// decryptLegacyCBC decrypts data using FileAPI legacy CBC format
func (s *S3Client) decryptLegacyCBC(encryptedData []byte, password string) ([]byte, error) {
	// Format: magic(8) + hash(32) + length(8) + salt(16) + iv(16) + encrypted_data
	if len(encryptedData) < 8+32+8+16+16 {
		return nil, fmt.Errorf("legacy CBC data too short: %d bytes", len(encryptedData))
	}

	// Extract stored hash
	storedHash := encryptedData[8:40]

	// Extract length
	lengthBytes := encryptedData[40:48]
	length := binary.BigEndian.Uint64(lengthBytes)

	// Extract encrypted portion (salt + iv + encrypted_data)
	encrypted := encryptedData[48:]

	if len(encrypted) != int(length) {
		return nil, fmt.Errorf("length mismatch: expected %d, got %d", length, len(encrypted))
	}

	// Verify hash integrity
	calculatedHash := sha256.Sum256(encrypted)
	if !bytes.Equal(storedHash, calculatedHash[:]) {
		return nil, fmt.Errorf("hash verification failed - data corrupted")
	}

	// Extract salt, IV, and encrypted data
	salt := encrypted[:16]
	iv := encrypted[16:32]
	ciphertext := encrypted[32:]

	// Derive key using PBKDF2 (same as FileAPI)
	key := pbkdf2.Key([]byte(password), salt, 100000, 32, sha256.New)

	// Create AES-CBC cipher
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	// Decrypt using CBC mode
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext is not a multiple of block size")
	}

	mode := cipher.NewCBCDecrypter(block, iv)
	plaintext := make([]byte, len(ciphertext))
	mode.CryptBlocks(plaintext, ciphertext)

	// Try to remove PKCS7 padding
	unpadded, err := s.removePKCS7Padding(plaintext)
	if err != nil {
		log.Warn().Err(err).Msg("PKCS7 unpadding failed, using raw data (old format)")
		// For backward compatibility with old files that didn't use padding
		return plaintext, nil
	}

	log.Debug().Msg("successfully decrypted using legacy CBC format")
	return unpadded, nil
}

// decryptLegacyGCM decrypts data using old GCM format without magic number
func (s *S3Client) decryptLegacyGCM(encryptedData []byte, password string) ([]byte, error) {
	// Original Goling format: salt(16) + nonce(12) + encrypted_data
	if len(encryptedData) < 28 {
		return nil, fmt.Errorf("legacy GCM data too short: %d bytes", len(encryptedData))
	}

	// Extract salt, nonce, and ciphertext
	salt := encryptedData[:16]
	nonce := encryptedData[16:28]
	ciphertext := encryptedData[28:]

	// Derive key from password using PBKDF2
	key := pbkdf2.Key([]byte(password), salt, 100000, 32, sha256.New)

	// Create AES cipher
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	// Create GCM mode
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	// Decrypt data
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("legacy GCM decryption failed: %w", err)
	}

	log.Debug().Msg("successfully decrypted using legacy GCM format")
	return plaintext, nil
}

// removePKCS7Padding removes PKCS7 padding from decrypted data
func (s *S3Client) removePKCS7Padding(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty data")
	}

	paddingLength := int(data[len(data)-1])
	if paddingLength == 0 || paddingLength > aes.BlockSize || paddingLength > len(data) {
		return nil, fmt.Errorf("invalid padding length: %d", paddingLength)
	}

	// Check that all padding bytes are the same
	for i := len(data) - paddingLength; i < len(data); i++ {
		if data[i] != byte(paddingLength) {
			return nil, fmt.Errorf("invalid padding at position %d", i)
		}
	}

	return data[:len(data)-paddingLength], nil
}

// UploadFile uploads an encrypted file to S3 (uses CBC format for Ghost Server compatibility)
func (s *S3Client) UploadFile(ctx context.Context, key string, data []byte, password string, metadata *FileMetadata) error {
	// Use Ghost Server compatible CBC format
	encryptionFormat := "3NCR0PTD"
	if metadata != nil && metadata.EncryptionFormat != "" {
		encryptionFormat = metadata.EncryptionFormat
	}

	// DEBUG: Log before encryption
	preview := string(data)
	if len(preview) > 200 {
		preview = preview[:200] + "..."
	}
	log.Debug().
		Str("key", key).
		Int("data_length", len(data)).
		Str("data_preview", preview).
		Bool("has_password", password != "").
		Msg("UploadFile: before encryption")

	// Encrypt the data
	encryptedData, err := s.encryptLegacyCBC(data, password)
	if err != nil {
		log.Error().Err(err).Str("key", key).Msg("UploadFile: encryption failed")
		return fmt.Errorf("failed to encrypt data: %w", err)
	}

	log.Debug().
		Str("key", key).
		Int("plaintext_length", len(data)).
		Int("encrypted_length", len(encryptedData)).
		Msg("UploadFile: after encryption")

	// Prepare S3 metadata
	s3Metadata := make(map[string]string)
	if metadata != nil {
		s3Metadata["name"] = metadata.OriginalName
		s3Metadata["content-type"] = metadata.ContentType
		s3Metadata["encrypted"] = "true"
		s3Metadata["encryption-format"] = encryptionFormat

		// Add custom metadata
		for k, v := range metadata.Metadata {
			s3Metadata[k] = v
		}
	}

	// Upload to S3
	log.Debug().
		Str("key", key).
		Int("encrypted_size", len(encryptedData)).
		Interface("metadata", s3Metadata).
		Msg("UploadFile: calling PutObject")

	output, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:   &s.bucketName,
		Key:      &key,
		Body:     bytes.NewReader(encryptedData),
		Metadata: s3Metadata,
	})

	if err != nil {
		log.Error().Err(err).Str("key", key).Msg("UploadFile: PutObject failed")
		return fmt.Errorf("failed to upload to S3: %w", err)
	}

	log.Debug().
		Str("key", key).
		Str("etag", *output.ETag).
		Msg("UploadFile: PutObject successful")

	log.Info().Str("key", key).Str("encryption", encryptionFormat).Msg("uploaded encrypted file to S3")
	return nil
}

// ListNextVersion returns the next available integer suffix for a base key using pattern baseKey_v{N}
func (s *S3Client) ListNextVersion(ctx context.Context, baseKey string) (int, error) {
    if baseKey == "" {
        return 1, nil
    }

    prefix := baseKey + "_v"
    maxVersion := 0

    paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
        Bucket: aws.String(s.bucketName),
        Prefix: aws.String(prefix),
    })

    for paginator.HasMorePages() {
        page, err := paginator.NextPage(ctx)
        if err != nil {
            return 1, fmt.Errorf("list versions failed: %w", err)
        }
        for _, obj := range page.Contents {
            if obj.Key == nil { continue }
            key := *obj.Key
            if strings.HasPrefix(key, prefix) {
                verStr := strings.TrimPrefix(key, prefix)
                if n, err := strconv.Atoi(verStr); err == nil {
                    if n > maxVersion { maxVersion = n }
                }
            }
        }
    }

    return maxVersion + 1, nil
}

// CopyObjectWithMetadata copies an object to a new key and replaces metadata
func (s *S3Client) CopyObjectWithMetadata(ctx context.Context, srcKey, dstKey string, meta map[string]string) error {
    if srcKey == "" || dstKey == "" {
        return fmt.Errorf("copy: empty src or dst key")
    }

    copySource := fmt.Sprintf("%s/%s", s.bucketName, srcKey)

    _, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
        Bucket:            aws.String(s.bucketName),
        Key:               aws.String(dstKey),
        CopySource:        aws.String(copySource),
        Metadata:          meta,
        MetadataDirective: s3types.MetadataDirectiveReplace,
    })
    if err != nil {
        return fmt.Errorf("copy object failed: %w", err)
    }
    log.Info().Str("src", srcKey).Str("dst", dstKey).Msg("copied object with replaced metadata")
    return nil
}

// encryptLegacyCBC encrypts data using FileAPI legacy CBC format for compatibility
func (s *S3Client) encryptLegacyCBC(data []byte, password string) ([]byte, error) {
	// Generate random salt and IV
	salt := make([]byte, 16)
	iv := make([]byte, 16) // AES block size

	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("failed to generate salt: %w", err)
	}
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, fmt.Errorf("failed to generate IV: %w", err)
	}

	// Derive key using PBKDF2 (same as FileAPI)
	key := pbkdf2.Key([]byte(password), salt, 100000, 32, sha256.New)

	// Create AES cipher
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	// Apply PKCS7 padding
	paddedData := applyPKCS7Padding(data, aes.BlockSize)

	// Encrypt using CBC mode
	mode := cipher.NewCBCEncrypter(block, iv)
	ciphertext := make([]byte, len(paddedData))
	mode.CryptBlocks(ciphertext, paddedData)

	// Combine salt + iv + ciphertext
	encrypted := make([]byte, 0, 16+16+len(ciphertext))
	encrypted = append(encrypted, salt...)
	encrypted = append(encrypted, iv...)
	encrypted = append(encrypted, ciphertext...)

	// Calculate hash of encrypted data
	hash := sha256.Sum256(encrypted)

	// Format: magic(8) + hash(32) + length(8) + salt(16) + iv(16) + encrypted_data
	magicNumber := []byte("3NCR0PTD")
	lengthBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(lengthBytes, uint64(len(encrypted)))

	result := make([]byte, 0, len(magicNumber)+32+8+len(encrypted))
	result = append(result, magicNumber...)
	result = append(result, hash[:]...)
	result = append(result, lengthBytes...)
	result = append(result, encrypted...)

	return result, nil
}

// applyPKCS7Padding applies PKCS7 padding to data
func applyPKCS7Padding(data []byte, blockSize int) []byte {
	padding := blockSize - (len(data) % blockSize)
	padText := bytes.Repeat([]byte{byte(padding)}, padding)
	return append(data, padText...)
}
