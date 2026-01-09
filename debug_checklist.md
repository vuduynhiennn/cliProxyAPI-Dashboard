# Cursor Request Sanitization Debug Checklist

## STATUS: 🔍 ROOT CAUSE IDENTIFIED - STREAMING RESPONSE BUG

**Last Updated:** 2026-01-09T11:24:00+07:00

---

## Root Cause Analysis

### Streaming Response Issues Found

Looking at the actual streaming output:

```
data: {...,"tool_calls":[{"id":"write-123","function":{...}}],"finish_reason":"tool_calls"}
data: {...,"finish_reason":"stop"} ← PROBLEM: Extra stop chunk after tool_calls!
data: [DONE]
```

| # | Issue | Current | Expected | Impact |
|---|-------|---------|----------|--------|
| 1 | **Duplicate finish_reason** | `tool_calls` then `stop` | Only `tool_calls` | Cursor sees `stop` and ignores tool call |
| 2 | **Tool call ID format** | `write-123456789-4` | `call_xxx` (OpenAI format) | May fail ID validation |
| 3 | **Extra empty chunk after tool_calls** | Sends empty delta with `stop` | Should not send | Overwrites tool_calls finish |

---

## Current Request Transformations (All ✅ Working)

| # | Issue | Status |
|---|-------|--------|
| 1 | tool_use → tool_calls | ✅ FIXED |
| 2 | tool_result → role:"tool" | ✅ FIXED |
| 3 | tool_result.content array extraction | ✅ FIXED |
| 4 | cache_control stripping | ✅ FIXED |
| 5 | additionalProperties stripping | ✅ FIXED |
| 6 | tool_choice normalization | ✅ FIXED |

---

## Response Transformations (Need Fixes)

| # | Issue | File | Status |
|---|-------|------|--------|
| 1 | Duplicate finish_reason chunks | `gemini_openai_response.go` | 🔴 TO FIX |
| 2 | Tool call ID format | `gemini_openai_response.go` | 🟡 OPTIONAL |

---

## Fix Plan

### Fix 1: Prevent duplicate finish_reason in streaming
- After sending `finish_reason: "tool_calls"`, do NOT send another chunk with `finish_reason: "stop"`
- Track state in the response converter to skip subsequent finish_reason chunks

### Fix 2 (Optional): Tool call ID format
- Change ID format from `{name}-{timestamp}-{counter}` to `call_{random}`
- Match OpenAI's ID format: `call_abc123...`

---

## Test Commands

```bash
# Streaming test
curl -s --max-time 20 -X POST http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer Ginstudio@1" \
  -H "Content-Type: application/json" \
  -d '{"model":"gemini-claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"use write tool to create test.md with hello"}],"tools":[{"type":"function","function":{"name":"write","description":"Writes file","parameters":{"type":"object","properties":{"file_path":{"type":"string"},"contents":{"type":"string"}},"required":["file_path","contents"]}}}]}'

# Active tunnel
https://tagged-qualify-curriculum-stake.trycloudflare.com
```

---

## Files to Modify

1. `internal/translator/gemini/openai/chat-completions/gemini_openai_response.go`
   - Fix streaming to not send extra `stop` after `tool_calls`
   - Optionally fix tool call ID format
