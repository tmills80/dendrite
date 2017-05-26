// Copyright 2017 Vector Creations Ltd
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fileutils

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/matrix-org/dendrite/mediaapi/types"
)

// FIXME: make into error types
var (
	// ErrFileIsTooLarge indicates that the uploaded file is larger than the configured maximum file size
	ErrFileIsTooLarge = fmt.Errorf("file is too large")
	errRead           = fmt.Errorf("failed to read response from remote server")
	errResponse       = fmt.Errorf("failed to write file data to response body")
	errHash           = fmt.Errorf("failed to hash file data")
	errWrite          = fmt.Errorf("failed to write file to disk")
)

// GetPathFromBase64Hash evaluates the path to a media file from its Base64Hash
// If the Base64Hash is long enough, we split it into pieces, creating up to 2 subdirectories
// for more manageable browsing and use the remainder as the file name.
// For example, if Base64Hash is 'qwerty', the path will be 'q/w/erty'.
func GetPathFromBase64Hash(base64Hash types.Base64Hash, absBasePath types.Path) (string, error) {
	var subPath, fileName string

	hashLen := len(base64Hash)

	switch {
	case hashLen < 1:
		return "", fmt.Errorf("Invalid filePath (Base64Hash too short): %q", base64Hash)
	case hashLen > 255:
		return "", fmt.Errorf("Invalid filePath (Base64Hash too long - max 255 characters): %q", base64Hash)
	case hashLen < 2:
		subPath = ""
		fileName = string(base64Hash)
	case hashLen < 3:
		subPath = string(base64Hash[0:1])
		fileName = string(base64Hash[1:])
	default:
		subPath = path.Join(
			string(base64Hash[0:1]),
			string(base64Hash[1:2]),
		)
		fileName = string(base64Hash[2:])
	}

	filePath, err := filepath.Abs(path.Join(
		string(absBasePath),
		subPath,
		fileName,
	))
	if err != nil {
		return "", fmt.Errorf("Unable to construct filePath: %q", err)
	}

	// check if the absolute absBasePath is a prefix of the absolute filePath
	// if so, no directory escape has occurred and the filePath is valid
	// Note: absBasePath is already absolute
	if strings.HasPrefix(filePath, string(absBasePath)) == false {
		return "", fmt.Errorf("Invalid filePath (not within absBasePath %v): %v", absBasePath, filePath)
	}

	return filePath, nil
}

// MoveFileWithHashCheck checks for hash collisions when moving a temporary file to its final path based on metadata
// The final path is based on the hash of the file.
// If the final path exists and the file size matches, the file does not need to be moved.
// In error cases where the file is not a duplicate, the caller may decide to remove the final path.
// Returns the final path of the file, whether it is a duplicate and an error.
func MoveFileWithHashCheck(tmpDir types.Path, mediaMetadata *types.MediaMetadata, absBasePath types.Path, logger *log.Entry) (types.Path, bool, error) {
	// Note: in all error and success cases, we need to remove the temporary directory
	defer RemoveDir(tmpDir, logger)
	duplicate := false
	finalPath, err := GetPathFromBase64Hash(mediaMetadata.Base64Hash, absBasePath)
	if err != nil {
		return "", duplicate, fmt.Errorf("failed to get file path from metadata: %q", err)
	}

	var stat os.FileInfo
	if stat, err = os.Stat(finalPath); os.IsExist(err) {
		duplicate = true
		if stat.Size() == int64(mediaMetadata.FileSizeBytes) {
			return types.Path(finalPath), duplicate, nil
		}
		return "", duplicate, fmt.Errorf("downloaded file with hash collision but different file size (%v)", finalPath)
	}
	err = moveFile(
		types.Path(path.Join(string(tmpDir), "content")),
		types.Path(finalPath),
	)
	if err != nil {
		return "", duplicate, fmt.Errorf("failed to move file to final destination (%v): %q", finalPath, err)
	}
	return types.Path(finalPath), duplicate, nil
}

// RemoveDir removes a directory and logs a warning in case of errors
func RemoveDir(dir types.Path, logger *log.Entry) {
	dirErr := os.RemoveAll(string(dir))
	if dirErr != nil {
		logger.WithError(dirErr).WithField("dir", dir).Warn("Failed to remove directory")
	}
}

// WriteTempFile writes to a new temporary file
func WriteTempFile(reqReader io.Reader, maxFileSizeBytes types.FileSizeBytes, absBasePath types.Path) (types.Base64Hash, types.FileSizeBytes, types.Path, error) {
	tmpFileWriter, tmpFile, tmpDir, err := createTempFileWriter(absBasePath)
	if err != nil {
		return "", -1, "", err
	}
	defer tmpFile.Close()

	limitedReader := io.LimitReader(reqReader, int64(maxFileSizeBytes))
	// Hash the file data. The hash will be returned. The hash is useful as a
	// method of deduplicating files to save storage, as well as a way to conduct
	// integrity checks on the file data in the repository.
	hasher := sha256.New()
	teeReader := io.TeeReader(limitedReader, hasher)
	bytesWritten, err := io.Copy(tmpFileWriter, teeReader)
	if err != nil && err != io.EOF {
		return "", -1, "", err
	}

	tmpFileWriter.Flush()

	hash := hasher.Sum(nil)
	return types.Base64Hash(base64.RawURLEncoding.EncodeToString(hash[:])), types.FileSizeBytes(bytesWritten), tmpDir, nil
}

// moveFile attempts to move the file src to dst
func moveFile(src types.Path, dst types.Path) error {
	dstDir := path.Dir(string(dst))

	err := os.MkdirAll(dstDir, 0770)
	if err != nil {
		return fmt.Errorf("Failed to make directory: %q", err)
	}
	err = os.Rename(string(src), string(dst))
	if err != nil {
		return fmt.Errorf("Failed to move directory: %q", err)
	}
	return nil
}

func createTempFileWriter(absBasePath types.Path) (*bufio.Writer, *os.File, types.Path, error) {
	tmpDir, err := createTempDir(absBasePath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("Failed to create temp dir: %q", err)
	}
	writer, tmpFile, err := createFileWriter(tmpDir, "content")
	if err != nil {
		return nil, nil, "", fmt.Errorf("Failed to create file writer: %q", err)
	}
	return writer, tmpFile, tmpDir, nil
}

// createTempDir creates a tmp/<random string> directory within baseDirectory and returns its path
func createTempDir(baseDirectory types.Path) (types.Path, error) {
	baseTmpDir := path.Join(string(baseDirectory), "tmp")
	if err := os.MkdirAll(baseTmpDir, 0770); err != nil {
		return "", fmt.Errorf("Failed to create base temp dir: %v", err)
	}
	tmpDir, err := ioutil.TempDir(baseTmpDir, "")
	if err != nil {
		return "", fmt.Errorf("Failed to create temp dir: %v", err)
	}
	return types.Path(tmpDir), nil
}

// createFileWriter creates a buffered file writer with a new file at directory/filename
// The caller should flush the writer before closing the file.
// Returns the file handle as it needs to be closed when writing is complete
func createFileWriter(directory types.Path, filename types.Filename) (*bufio.Writer, *os.File, error) {
	filePath := path.Join(string(directory), string(filename))
	file, err := os.Create(filePath)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to create file: %v", err)
	}

	return bufio.NewWriter(file), file, nil
}
