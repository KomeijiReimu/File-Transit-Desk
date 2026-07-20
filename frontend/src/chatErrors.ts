import { ApiError } from '@/api'

export type ChatErrorContext = 'capabilities' | 'history' | 'sync' | 'send' | 'withdraw' | 'delete' | 'batch-delete' | 'clear'

function retryAfterText(error: ApiError) {
  if (error.retryAfter === undefined) return ''
  if (error.retryAfter <= 1) return '，请稍后重试'
  if (error.retryAfter < 60) return `，请在 ${error.retryAfter} 秒后重试`
  return `，请在 ${Math.ceil(error.retryAfter / 60)} 分钟后重试`
}

export function chatErrorMessage(error: unknown, context: ChatErrorContext) {
  if (!(error instanceof ApiError)) {
    if (context === 'sync') return '消息更新暂时中断，正在自动重试。'
    if (context === 'capabilities') return '无法读取聊天发送限制，请重试。'
    if (context === 'history') return '聊天记录加载失败，请重试。'
    return '操作未完成，请重试。'
  }

  const retry = retryAfterText(error)
  switch (error.code) {
    case 'chat_send_rate_limited':
      return `发送消息过于频繁${retry || '，请稍后重试'}。`
    case 'chat_action_rate_limited':
      return `聊天操作过于频繁${retry || '，请稍后重试'}。`
    case 'chat_withdraw_expired':
      return '这条消息已超过可撤回时间。'
    case 'chat_withdraw_forbidden':
      return '你无权撤回这条消息。'
    case 'chat_state_conflict':
      return '消息状态已经变化，请同步后再试。'
    case 'chat_message_not_found':
      return '这条消息已不存在。'
    case 'chat_request_too_large':
      return '消息请求超过服务器允许的大小，请缩短内容。'
    case 'chat_message_too_large':
      return '消息正文超过服务器允许的长度，请缩短内容。'
    case 'chat_message_empty':
      return '消息不能为空。'
    case 'chat_message_control_character':
      return '消息包含不安全的控制字符，请移除后再发送。'
    case 'chat_message_invalid':
    case 'chat_request_invalid':
      return '消息格式无效，请检查内容后重试。'
    case 'chat_content_type_invalid':
      return '服务器未接受消息格式，请刷新页面后重试。'
    case 'chat_page_invalid':
      return '聊天分页参数已失效，请重新加载聊天记录。'
    case 'chat_generation_invalid':
      return '聊天同步参数已失效，请重新加载聊天记录。'
    case 'chat_cursor_reset_required':
      return '聊天记录状态已变化，正在重新加载。'
    case 'chat_batch_delete_conflict':
      return '消息状态已更新，请同步后重试。'
    case 'chat_batch_delete_request_invalid':
      return '所选消息无效，请重新选择。'
    case 'chat_batch_delete_request_too_large':
      return '所选消息过多，请减少后重试。'
    case 'chat_batch_delete_content_type_invalid':
      return '批量删除请求无效，请刷新后重试。'
    case 'chat_clear_conflict':
      return '消息已更新，请重新确认清空。'
    case 'chat_clear_request_invalid':
    case 'chat_clear_content_type_invalid':
      return '清空请求无效，请刷新后重试。'
    case 'chat_clear_request_too_large':
      return '清空请求过大，请刷新后重试。'
    default:
      break
  }

  if (error.status === 0) {
    return context === 'sync'
      ? '消息更新暂时中断，正在自动重试。'
      : '无法连接服务器，请检查网络后重试。'
  }
  if (error.status === 413) return '消息超过服务器允许的大小，请缩短内容。'
  if (error.status === 429) return `请求过于频繁${retry || '，请稍后重试'}。`
  if (context === 'sync') return '消息更新暂时中断，正在自动重试。'
  return error.message || '操作未完成，请重试。'
}
