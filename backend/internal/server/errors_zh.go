package server

import "net/http"

func localizedHTTPError(status int, message string) string {
	if message != http.StatusText(status) {
		return message
	}
	switch status {
	case 400:
		return "请求参数无效，请检查后重试。"
	case 401:
		return "登录状态或访问凭据无效，请重新登录或获取新的访问链接。"
	case 403:
		return "没有执行此操作的权限。"
	case 404:
		return "请求的内容不存在。"
	case 405:
		return "此接口不支持该请求方式。"
	case 408:
		return "请求超时，请重试。"
	case 409:
		return "当前状态与请求冲突，请刷新后重试。"
	case 411:
		return "请求缺少内容长度。"
	case 413:
		return "上传内容超出大小限制。"
	case 415:
		return "不支持此内容类型。"
	case 416:
		return "请求的文件范围无效。"
	case 422:
		return "请求内容无法处理。"
	case 429:
		return "请求过于频繁，请稍后重试。"
	case 500:
		return "服务暂时不可用，请稍后重试。"
	case 502:
		return "上游服务响应异常。"
	case 503:
		return "服务暂时繁忙，请稍后重试。"
	case 504:
		return "上游服务响应超时。"
	default:
		return "请求无法完成，请稍后重试。"
	}
}
