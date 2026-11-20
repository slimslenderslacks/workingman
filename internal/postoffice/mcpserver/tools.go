package mcpserver

// Tool input schemas and descriptions, as specified in
// docs/messaging-design.md §4.4. The wait maxima ("50") are filled in from
// Config.MaxWait (the --max-wait flag) so the schema, the description and
// the cap agree.

const sendNotificationSchema = `{
  "type": "object",
  "properties": {
    "subject":   {"type": "string", "maxLength": 200, "description": "One-line summary shown in inbox lists and notifications."},
    "body":      {"type": "string", "maxLength": 16000, "description": "The message, in markdown."},
    "recipient": {"type": "string", "description": "Who to notify. Omit for the human operator, or give an agent name such as \"step-up\"."}
  },
  "required": ["subject", "body"]
}`

const sendNotificationDesc = "Send a one-way notification. No reply is expected and this call returns " +
	"immediately. Use it for status updates, FYIs, or to say that you took an " +
	"action someone should know about. `recipient` defaults to the human " +
	"operator; set it to an agent name (for example `\"step-up\"`) to notify that " +
	"agent instead. Do not use this when you need an answer — use `send_request`."

// sendRequestSchema takes the wait maximum and default, and extra
// properties ("" or inReplyToProperty).
const sendRequestSchema = `{
  "type": "object",
  "properties": {
    "body":               {"type": "string", "maxLength": 16000, "description": "The question, with enough context for the recipient to answer without seeing your work."},
    "subject":            {"type": "string", "maxLength": 200, "description": "One-line summary. Defaults to the first line of body."},
    "recipient":          {"type": "string", "description": "Omit for the human operator, or give an agent name such as \"step-up\"."},
    "options":            {"type": "array", "items": {"type": "string", "maxLength": 100}, "maxItems": 10, "description": "Optional suggested answers. The recipient may still answer freely."},
    "wait_seconds":       {"type": "integer", "minimum": 0, "maximum": %[1]d, "default": %[2]d, "description": "How long this call waits for an answer before returning pending."},
    "expires_in_minutes": {"type": "integer", "minimum": 1, "maximum": 10080, "default": 1440, "description": "How long the request stays open before it expires."}%[3]s
  },
  "required": ["body"]
}`

// sendRequestDesc takes the wait maximum twice.
const sendRequestDesc = "Ask a question that needs an answer before you can continue: missing " +
	"information, an approval, or help. `recipient` defaults to the human " +
	"operator; set it to an agent name (for example `\"step-up\"`) to ask that " +
	"agent.\n\n" +
	"This call waits up to `wait_seconds` (max %[1]d) for an answer.\n" +
	"- If the first line of the result is `status: answered`, the reply is in\n" +
	"  `response`.\n" +
	"- If it is `status: pending`, the question has been delivered and is still\n" +
	"  open. Keep the `request_id`. The answer may take minutes or hours.\n\n" +
	"**To wait for the answer**, call `wait_for_response` with the `request_id`. " +
	"Each call waits up to about %[2]d seconds. If it returns `status: pending`, " +
	"call it again, for as long as you are willing to wait. If you can make " +
	"progress on other work meanwhile, do that and call `check_response` " +
	"occasionally instead.\n\n" +
	"**If you decide to stop waiting**, call `cancel_request` with the " +
	"`request_id`, so the recipient knows you no longer need an answer. Then " +
	"continue with your best judgement and say in your output that the request " +
	"went unanswered.\n\n" +
	"The request expires automatically after `expires_in_minutes` (default " +
	"24 h). Do not send the same question twice; reuse the `request_id` you " +
	"have. If you lose it, call `list_requests`."

// waitSchema takes the wait maximum and default.
const waitSchema = `{
  "type": "object",
  "properties": {
    "request_id":      {"type": "string", "description": "The request_id returned by send_request."},
    "timeout_seconds": {"type": "integer", "minimum": 1, "maximum": %[1]d, "default": %[2]d}
  },
  "required": ["request_id"]
}`

// waitDesc takes the wait maximum.
const waitDesc = "Wait up to `timeout_seconds` (max %d) for the answer to a request you made " +
	"with `send_request`.\n" +
	"- `status: answered`: the reply is in `response`.\n" +
	"- `status: pending`: no answer yet. Call `wait_for_response` again to keep\n" +
	"  waiting, or `cancel_request` to give up.\n" +
	"- `status: expired` or `status: cancelled`: no answer will come. Continue\n" +
	"  without it.\n\n" +
	"If this call is interrupted or times out, the request is **not** cancelled. " +
	"Just call `wait_for_response` again."

const checkSchema = `{
  "type": "object",
  "properties": {
    "request_id": {"type": "string", "description": "The request_id returned by send_request."}
  },
  "required": ["request_id"]
}`

const checkDesc = "Check, without waiting, whether a request made with `send_request` has been " +
	"answered. Use it between other work; use `wait_for_response` when you have " +
	"nothing else to do."

const cancelSchema = `{
  "type": "object",
  "properties": {
    "request_id": {"type": "string"},
    "reason":     {"type": "string", "maxLength": 500, "description": "Shown to the recipient, e.g. \"found the answer in the docs\"."}
  },
  "required": ["request_id"]
}`

const cancelDesc = "Stop waiting for a request made with `send_request`. The recipient is told " +
	"you no longer need an answer, and the request leaves the operator's inbox. " +
	"If an answer had already arrived, this returns it with `status: answered` " +
	"instead."

const listSchema = `{
  "type": "object",
  "properties": {
    "status": {"type": "string", "enum": ["open", "closed", "all"], "default": "open"}
  }
}`

const listDesc = "List the requests you have sent, so you can recover a `request_id` you have " +
	"lost track of (for example after your context was summarised)."

// inReplyToProperty is send_request's extra property for registered
// recipients (§4.5): sendRequestSchema's third argument for them, "" for
// everyone else.
const inReplyToProperty = `,
    "in_reply_to":        {"type": "string", "description": "The request_id of a request you received that this request serves, e.g. when you ask the operator to approve what an agent asked you for. Set automatically when you hold exactly one received, unanswered request."}`

// sendRequestRecipientDesc is appended to send_request's description for
// registered recipients.
const sendRequestRecipientDesc = "\n\n" +
	"When this request serves a request you received with `next_message` (for " +
	"example, asking the operator to approve what an agent asked you for), it is " +
	"linked to that request with `in_reply_to`. If you hold exactly one received, " +
	"unanswered request, the link is made for you; if you hold several, pass " +
	"`in_reply_to` with the one this request serves."

// recipientsOnly starts each receiver tool's description: a gateway may list
// them for any agent (see senderTools), but only a registered recipient may
// call them.
const recipientsOnly = "For agents that receive messages (such as step-up); anyone else gets status: rejected. "

// nextMessageSchema takes the wait maximum and default.
const nextMessageSchema = `{
  "type": "object",
  "properties": {
    "timeout_seconds": {"type": "integer", "minimum": 0, "maximum": %[1]d, "default": %[2]d}
  }
}`

// nextMessageDesc takes the wait maximum.
const nextMessageDesc = recipientsOnly + "Fetch the next message addressed to you. Waits up to `timeout_seconds` (max " +
	"%d) and returns `status: empty` if nothing arrives; call it again to keep " +
	"listening. Each message is returned once. A `kind: request` must be answered " +
	"with `reply`, or it will expire and the sender will carry on without you. " +
	"A `kind: notification` needs no reply and is closed once you receive it. " +
	"Use `list_inbox` to see messages you have received but not yet answered."

const replySchema = `{
  "type": "object",
  "properties": {
    "request_id": {"type": "string", "description": "The request_id returned by next_message or list_inbox."},
    "response":   {"type": "string", "maxLength": 16000, "description": "Your answer, returned to the sender's wait_for_response."}
  },
  "required": ["request_id", "response"]
}`

const replyDesc = recipientsOnly + "Answer a request you received from `next_message`. The sender's " +
	"`wait_for_response` returns your `response`. If the request was already " +
	"closed (cancelled by the sender, or expired), this tells you so."

const listInboxSchema = `{
  "type": "object",
  "properties": {}
}`

const listInboxDesc = recipientsOnly + "List the open messages addressed to you, including ones you have already " +
	"received but not yet answered. Use it after a restart to pick up work in " +
	"progress."
