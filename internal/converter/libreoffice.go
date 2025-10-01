package converter

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// LibreOffice handles document conversion using LibreOffice
type LibreOffice struct {
	serverRunning bool
	serverMutex   sync.RWMutex
	port          int
	maxWorkers    int
	semaphore     chan struct{}
}

// Job represents a document conversion job
type Job struct {
	InputPath  string
	OutputPath string
	Extension  string
	Timeout    time.Duration
}

// Result represents the result of a conversion operation
type Result struct {
	Success     bool
	OutputPath  string
	Error       string
	Duration    time.Duration
	IsProtected bool
}

// NewLibreOffice creates a new LibreOffice converter instance
func NewLibreOffice(port int, maxWorkers int) *LibreOffice {
	return &LibreOffice{
		port:       port,
		maxWorkers: maxWorkers,
		semaphore:  make(chan struct{}, maxWorkers),
	}
}

// Initialize starts the LibreOffice server and sets up the converter
func (l *LibreOffice) Initialize() error {
	log.Info().Int("port", l.port).Int("max_workers", l.maxWorkers).Msg("initializing LibreOffice converter")

	// Check if LibreOffice is installed
	if err := l.checkInstallation(); err != nil {
		return fmt.Errorf("LibreOffice not available: %w", err)
	}

	// Start the LibreOffice server
	if err := l.startServer(); err != nil {
		return fmt.Errorf("failed to start LibreOffice server: %w", err)
	}

	log.Info().Msg("LibreOffice converter initialized successfully")
	return nil
}

// checkInstallation verifies LibreOffice is available
func (l *LibreOffice) checkInstallation() error {
	cmd := exec.Command("libreoffice", "--version")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("LibreOffice not found in PATH: %w", err)
	}

	log.Info().Str("version", strings.TrimSpace(string(output))).Msg("LibreOffice found")
	return nil
}

// startServer starts the LibreOffice server in headless mode
func (l *LibreOffice) startServer() error {
	l.serverMutex.Lock()
	defer l.serverMutex.Unlock()

	if l.serverRunning {
		return nil
	}

	// Kill any existing LibreOffice processes
	exec.Command("pkill", "-f", "libreoffice").Run()
	time.Sleep(1 * time.Second)

	// Start LibreOffice server
	cmd := exec.Command(
		"libreoffice",
		"--headless",
		fmt.Sprintf("--accept=socket,host=localhost,port=%d;urp;", l.port),
		"--nofirststartwizard",
		"--nologo",
		"--nolockcheck",
	)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start LibreOffice server: %w", err)
	}

	// Wait for server to be ready
	time.Sleep(3 * time.Second)

	// Verify server is running
	if err := l.checkConnection(); err != nil {
		return fmt.Errorf("LibreOffice server not responding: %w", err)
	}

	l.serverRunning = true
	log.Info().Int("port", l.port).Msg("LibreOffice server started")
	return nil
}

// checkConnection verifies the LibreOffice server is responding
func (l *LibreOffice) checkConnection() error {
	// Simple test conversion to verify server is working
	tempDir := os.TempDir()
	testFile := filepath.Join(tempDir, "test.txt")

	// Create a simple test file
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		return fmt.Errorf("failed to create test file: %w", err)
	}
	defer os.Remove(testFile)

	cmd := exec.Command(
		"libreoffice",
		"--headless",
		"--convert-to", "pdf",
		"--outdir", tempDir,
		testFile,
	)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("test conversion failed: %w", err)
	}

	// Clean up test output
	testPDF := filepath.Join(tempDir, "test.pdf")
	os.Remove(testPDF)

	return nil
}

// ConvertToPDF converts a document to PDF format
func (l *LibreOffice) ConvertToPDF(job Job) Result {
	startTime := time.Now()

	// Acquire semaphore to limit concurrent conversions
	l.semaphore <- struct{}{}
	defer func() { <-l.semaphore }()

	log.Info().Str("input", job.InputPath).Str("output", job.OutputPath).Msg("starting conversion")

	// Check if input file exists and is readable
	if err := l.validateInput(job.InputPath); err != nil {
		return Result{
			Success:  false,
			Error:    fmt.Sprintf("input validation failed: %v", err),
			Duration: time.Since(startTime),
		}
	}

	// Check for password protection
	if isProtected, err := l.checkPasswordProtection(job.InputPath, job.Extension); err != nil {
		log.Warn().Err(err).Str("file", job.InputPath).Msg("could not check password protection")
	} else if isProtected {
		return Result{
			Success:     false,
			Error:       "document is password protected",
			Duration:    time.Since(startTime),
			IsProtected: true,
		}
	}

	// Create unique profile directory for this conversion
	profileDir := filepath.Join(os.TempDir(), fmt.Sprintf("libreoffice_profile_%s", uuid.New().String()))
	if err := os.MkdirAll(profileDir, 0755); err != nil {
		return Result{
			Success:  false,
			Error:    fmt.Sprintf("failed to create profile directory: %v", err),
			Duration: time.Since(startTime),
		}
	}
	defer os.RemoveAll(profileDir)

	// Ensure output directory exists
	outputDir := filepath.Dir(job.OutputPath)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return Result{
			Success:  false,
			Error:    fmt.Sprintf("failed to create output directory: %v", err),
			Duration: time.Since(startTime),
		}
	}

	// Build conversion command
	cmd := exec.Command(
		"libreoffice",
		fmt.Sprintf("-env:UserInstallation=file://%s", profileDir),
		"--headless",
		"--convert-to", "pdf",
		"--outdir", outputDir,
		job.InputPath,
	)

	// Set timeout
	timeout := job.Timeout
	if timeout == 0 {
		timeout = 180 * time.Second // Default 3 minutes
	}

	log.Debug().Str("cmd", strings.Join(cmd.Args, " ")).Msg("LibreOffice command")

	// Run conversion with timeout
	done := make(chan error, 1)
	go func() {
		done <- cmd.Run()
	}()

	select {
	case err := <-done:
		if err != nil {
			return Result{
				Success:  false,
				Error:    fmt.Sprintf("conversion failed: %v", err),
				Duration: time.Since(startTime),
			}
		}
	case <-time.After(timeout):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return Result{
			Success:  false,
			Error:    fmt.Sprintf("conversion timeout after %v", timeout),
			Duration: time.Since(startTime),
		}
	}

	// Check if output file was created
	expectedOutput := l.getExpectedOutputPath(job.InputPath, outputDir)
	actualOutput := job.OutputPath

	if expectedOutput != actualOutput {
		// LibreOffice created file with input filename, need to rename
		if _, err := os.Stat(expectedOutput); err == nil {
			if err := os.Rename(expectedOutput, actualOutput); err != nil {
				log.Warn().Err(err).Str("from", expectedOutput).Str("to", actualOutput).Msg("failed to rename")
				actualOutput = expectedOutput // Use the file as created by LibreOffice
			}
		}
	}

	// Verify final output exists
	if _, err := os.Stat(actualOutput); err != nil {
		return Result{
			Success:  false,
			Error:    fmt.Sprintf("output file not created: %v", err),
			Duration: time.Since(startTime),
		}
	}

	log.Info().Str("output", actualOutput).Dur("duration", time.Since(startTime)).Msg("conversion successful")

	return Result{
		Success:    true,
		OutputPath: actualOutput,
		Duration:   time.Since(startTime),
	}
}

// validateInput checks if the input file is readable
func (l *LibreOffice) validateInput(filePath string) error {
	info, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("file not found: %w", err)
	}

	if info.IsDir() {
		return fmt.Errorf("path is a directory, not a file")
	}

	if info.Size() == 0 {
		return fmt.Errorf("file is empty")
	}

	// Check if file is readable
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("file not readable: %w", err)
	}
	file.Close()

	return nil
}

// checkPasswordProtection checks if a document is password protected
func (l *LibreOffice) checkPasswordProtection(filePath, extension string) (bool, error) {
	// Quick check using LibreOffice headless mode
	cmd := exec.Command(
		"libreoffice",
		"--headless",
		"--cat",
		filePath,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		outputStr := strings.ToLower(string(output))
		if strings.Contains(outputStr, "password") ||
			strings.Contains(outputStr, "encrypted") ||
			strings.Contains(outputStr, "protected") {
			return true, nil
		}
	}

	return false, nil
}

// getExpectedOutputPath calculates the path where LibreOffice will create the output file
func (l *LibreOffice) getExpectedOutputPath(inputPath, outputDir string) string {
	baseName := filepath.Base(inputPath)
	nameWithoutExt := strings.TrimSuffix(baseName, filepath.Ext(baseName))
	return filepath.Join(outputDir, nameWithoutExt+".pdf")
}

// SupportedExtensions returns a list of file extensions supported for conversion
func (l *LibreOffice) SupportedExtensions() []string {
	return []string{
		"doc", "docx", "rtf", "odt", // Word processing
		"xls", "xlsx", "ods", "csv", // Spreadsheets
		"ppt", "pptx", "odp",        // Presentations
		"vsd", "vsdx",               // Visio diagrams
		"txt", "html", "htm",        // Text/Web
	}
}

// IsSupported checks if a file extension is supported for conversion
func (l *LibreOffice) IsSupported(extension string) bool {
	ext := strings.ToLower(strings.TrimPrefix(extension, "."))
	supported := l.SupportedExtensions()

	for _, supportedExt := range supported {
		if ext == supportedExt {
			return true
		}
	}
	return false
}

// Shutdown gracefully shuts down the LibreOffice converter
func (l *LibreOffice) Shutdown() error {
	l.serverMutex.Lock()
	defer l.serverMutex.Unlock()

	if !l.serverRunning {
		return nil
	}

	log.Info().Msg("shutting down LibreOffice converter")

	// Kill LibreOffice processes
	cmd := exec.Command("pkill", "-f", "libreoffice")
	if err := cmd.Run(); err != nil {
		log.Warn().Err(err).Msg("failed to kill LibreOffice processes")
	}

	// Wait a moment for processes to terminate
	time.Sleep(1 * time.Second)

	l.serverRunning = false
	log.Info().Msg("LibreOffice converter shut down")
	return nil
}
