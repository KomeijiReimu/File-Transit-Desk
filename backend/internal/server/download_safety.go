package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"filetrans-backend/internal/config"
	"filetrans-backend/internal/store"

	"github.com/gofiber/fiber/v2"
)

var errDownloadFileNotRegular = errors.New("download target is not a regular file")
var errDownloadFileChanged = errors.New("download target changed during validation")
var errDownloadHashCapacity = errors.New("download hash capacity exhausted")
var errDownloadHashPanicked = errors.New("download hash verification panicked")

type downloadHashFlight struct {
	done    chan struct{}
	full    string
	info    os.FileInfo
	first   bool
	err     error
	waiters int
}

func fileSHA256Hex(path string) (string, error) {
	hash, _, err := fileSHA256HexWithInfo(path)
	return hash, err
}

func fileSHA256HexWithInfo(path string) (string, os.FileInfo, error) {
	return fileSHA256HexWithInfoHook(path, nil)
}

func fileSHA256HexWithInfoHook(path string, afterOpen func()) (string, os.FileInfo, error) {
	pathInfo, err := os.Stat(path)
	if err != nil {
		return "", nil, err
	}
	if !pathInfo.Mode().IsRegular() {
		return "", nil, errDownloadFileNotRegular
	}
	file, err := openDownloadFile(path)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return "", nil, err
	}
	if !before.Mode().IsRegular() {
		return "", nil, errDownloadFileNotRegular
	}
	if !os.SameFile(pathInfo, before) {
		return "", nil, errDownloadFileChanged
	}
	if afterOpen != nil {
		afterOpen()
	}
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return "", nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", nil, errDownloadFileChanged
	}
	pathAfter, err := os.Stat(path)
	if err != nil {
		return "", nil, errDownloadFileChanged
	}
	if !pathAfter.Mode().IsRegular() {
		return "", nil, errDownloadFileNotRegular
	}
	if !os.SameFile(after, pathAfter) || after.Size() != pathAfter.Size() || !after.ModTime().Equal(pathAfter.ModTime()) {
		return "", nil, errDownloadFileChanged
	}
	return hex.EncodeToString(h.Sum(nil)), after, nil
}

func downloadFileSafetyError(err error) error {
	if errors.Is(err, errDownloadFileNotRegular) {
		return newCodedAPIError(fiber.StatusConflict, "download_file_not_regular", "下载文件当前不可用，请重新获取下载链接。")
	}
	if errors.Is(err, errDownloadFileChanged) || errors.Is(err, os.ErrNotExist) {
		return newCodedAPIError(fiber.StatusConflict, "download_file_changed", "下载文件在校验期间发生变化，请重试。")
	}
	return err
}

func downloadHashRequestError(c *fiber.Ctx, err error) error {
	if errors.Is(err, errDownloadHashCapacity) {
		c.Set("Retry-After", "2")
		return newCodedAPIError(fiber.StatusServiceUnavailable, "download_hash_capacity_exhausted", "下载内容校验繁忙，请稍后重试。")
	}
	if errors.Is(err, errDownloadHashPanicked) {
		return newCodedAPIError(fiber.StatusInternalServerError, "download_hash_failed", "下载内容校验失败，请稍后重试。")
	}
	return downloadFileSafetyError(err)
}

func (s *Server) hashDownloadFile(path string) (string, os.FileInfo, error) {
	s.downloadHashMu.Lock()
	if s.downloadHashSlots == nil {
		limit := s.cfg().Downloads.MaxConcurrentHashes
		if limit < 1 {
			limit = 1
		}
		s.downloadHashSlots = make(chan struct{}, limit)
	}
	slots := s.downloadHashSlots
	s.downloadHashMu.Unlock()
	if s.beforeDownloadHashAcquire != nil {
		s.beforeDownloadHashAcquire()
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		return "", nil, errDownloadHashCapacity
	}
	return fileSHA256HexWithInfoHook(path, s.duringDownloadFileHash)
}

func (s *Server) verifyDownloadLeaseContent(c *fiber.Ctx, lease store.DownloadLease, full string, info os.FileInfo) (os.FileInfo, error) {
	if !lease.FileSHA256.Valid || strings.TrimSpace(lease.FileSHA256.String) == "" {
		return info, nil
	}
	currentHash, checkedInfo, err := s.hashDownloadFile(full)
	if err != nil {
		return nil, err
	}
	if s.afterDownloadFileHash != nil {
		s.afterDownloadFileHash()
	}
	if currentHash != lease.FileSHA256.String {
		s.criticalAudit("download_lease_file_changed", s.clientIP(c), "下载票据内容哈希不匹配")
		return nil, newCodedAPIError(fiber.StatusConflict, "download_file_changed", "文件内容已变化，请重新获取下载链接。")
	}
	return checkedInfo, nil
}

func (s *Server) ensureDownloadLeaseFirstUse(c *fiber.Ctx, lease store.DownloadLease, dir config.Dir, full string, info os.FileInfo) (string, os.FileInfo, bool, error) {
	s.downloadHashMu.Lock()
	if s.downloadHashFlights == nil {
		s.downloadHashFlights = make(map[string]*downloadHashFlight)
	}
	if existing := s.downloadHashFlights[lease.Hash]; existing != nil {
		existing.waiters++
		s.downloadHashMu.Unlock()
		<-existing.done
		if existing.err != nil {
			return existing.full, existing.info, false, existing.err
		}
		full, info, err := s.revalidateDownloadFile(dir, lease.Path, existing.info)
		return full, info, false, err
	}
	flight := &downloadHashFlight{done: make(chan struct{})}
	s.downloadHashFlights[lease.Hash] = flight
	s.downloadHashMu.Unlock()

	func() {
		defer func() {
			if recover() != nil {
				log.Printf("[CRITICAL] download hash flight panicked")
				flight.full = ""
				flight.info = nil
				flight.first = false
				flight.err = errDownloadHashPanicked
			}
			s.downloadHashMu.Lock()
			delete(s.downloadHashFlights, lease.Hash)
			close(flight.done)
			s.downloadHashMu.Unlock()
		}()
		flight.full, flight.info, flight.first, flight.err = s.runDownloadLeaseFirstUse(c, lease, dir, full, info)
	}()
	return flight.full, flight.info, flight.first, flight.err
}

func (s *Server) runDownloadLeaseFirstUse(c *fiber.Ctx, lease store.DownloadLease, dir config.Dir, full string, info os.FileInfo) (string, os.FileInfo, bool, error) {
	latest, err := s.store.DownloadLeaseByHash(lease.Hash)
	if err != nil {
		return "", nil, false, err
	}
	if latest.LastUsedAt.Valid {
		checkedInfo := info
		if s.cfg().Downloads.VerifyHashOnEveryRequest {
			checkedInfo, err = s.verifyDownloadLeaseContent(c, lease, full, info)
		}
		if err == nil {
			full, info, err = s.revalidateDownloadFile(dir, lease.Path, checkedInfo)
		}
		return full, info, false, err
	}
	checkedInfo, err := s.verifyDownloadLeaseContent(c, lease, full, info)
	if err != nil {
		return "", nil, false, err
	}
	full, info, err = s.revalidateDownloadFile(dir, lease.Path, checkedInfo)
	if err != nil {
		return "", nil, false, err
	}
	first, err := s.store.MarkDownloadLeaseFirstUsed(lease.Hash, time.Now())
	if err != nil {
		return "", nil, false, err
	}
	return full, info, first, nil
}

func (s *Server) revalidateDownloadFile(dir config.Dir, rel string, expected os.FileInfo) (string, os.FileInfo, error) {
	if s.beforeDownloadFinalValidation != nil {
		s.beforeDownloadFinalValidation()
	}
	full, _, current, err := s.resolveDownloadFile(dir, rel)
	if err != nil {
		return "", nil, downloadFileSafetyError(errDownloadFileChanged)
	}
	if expected == nil || !current.Mode().IsRegular() || !os.SameFile(expected, current) || expected.Size() != current.Size() || !expected.ModTime().Equal(current.ModTime()) {
		return "", nil, downloadFileSafetyError(errDownloadFileChanged)
	}
	return full, current, nil
}
