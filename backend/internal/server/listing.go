package server

import (
	"errors"
	"strconv"

	"filetrans-backend/internal/fsutil"

	"github.com/gofiber/fiber/v2"
)

type listingPage struct {
	Page     int64
	PageSize int64
}

func parseListingPage(c *fiber.Ctx, maxPageSize, scanLimit int, legacyAll bool) (listingPage, error) {
	page := int64(1)
	pageSize := int64(maxPageSize)
	pageRaw := string(c.Context().QueryArgs().Peek("page"))
	pageSizeRaw := string(c.Context().QueryArgs().Peek("pageSize"))
	if pageRaw == "" && pageSizeRaw == "" && legacyAll {
		pageSize = int64(scanLimit)
	}
	if pageRaw != "" {
		value, err := strconv.ParseInt(pageRaw, 10, 64)
		if err != nil || value < 1 {
			return listingPage{}, newCodedAPIError(fiber.StatusBadRequest, "directory_page_out_of_range", "目录分页参数超出扫描窗口。")
		}
		page = value
	}
	if pageSizeRaw != "" {
		value, err := strconv.ParseInt(pageSizeRaw, 10, 64)
		if err != nil || value < 1 {
			return listingPage{}, newCodedAPIError(fiber.StatusBadRequest, "directory_page_out_of_range", "目录分页参数超出扫描窗口。")
		}
		if value > int64(maxPageSize) {
			pageSize = int64(maxPageSize)
		} else {
			pageSize = value
		}
	}
	if pageSize < 1 {
		return listingPage{}, newCodedAPIError(fiber.StatusBadRequest, "directory_page_out_of_range", "目录分页参数超出扫描窗口。")
	}
	pageIndex := page - 1
	if pageIndex > int64(^uint64(0)>>1)/pageSize || pageIndex*pageSize >= int64(scanLimit) {
		return listingPage{}, newCodedAPIError(fiber.StatusBadRequest, "directory_page_out_of_range", "目录分页参数超出扫描窗口。")
	}
	return listingPage{Page: page, PageSize: pageSize}, nil
}

func mapListingError(err error) error {
	if errors.Is(err, fsutil.ErrPageOutOfRange) {
		return newCodedAPIError(fiber.StatusBadRequest, "directory_page_out_of_range", "目录分页参数超出扫描窗口。")
	}
	return err
}
