# Responses ↔ Chat Completions 工具转换设计说明

本文档用于实现一个 **Responses API ↔ 上游 Chat Completions-compatible Provider** 的桥接层，重点覆盖 `tools`、`tool_choice`、工具调用、工具结果、流式事件，以及 Responses 回传结构的双向转换。

适用场景：

- 客户端请求是 OpenAI Responses 风格；
- 上游只支持 `/chat/completions` 或 OpenAI-compatible Chat Completions；
- 需要兼容 DeepSeek、OpenAI-compatible Provider、vLLM、Ollama、OpenRouter 部分后端等；
- 需要避免上游因 `tools[].type = "custom"`、`web_search`、`file_search` 等 Responses-only 工具类型报错。

---

## 1. 核心原则

### 1.1 发往 Chat Completions 前只保留标准 function tools

多数 Chat Completions-compatible Provider 对 `tools` 的支持集中在：

```json
{
  "type": "function",
  "function": {
    "name": "tool_name",
    "description": "Tool description",
    "parameters": {
      "type": "object",
      "properties": {}
    }
  }
}
```

因此转换层应采用白名单策略：

```txt
只向上游发送 tools[].type = "function"
其他类型必须在 bridge 层处理、降级、展开或丢弃
```

不要把 Responses 工具原样透传给 Chat Completions 上游。

---

### 1.2 Responses 工具类型更丰富，不等价于 Chat tools

Responses 侧可能出现：

```txt
function
custom
web_search / web_search_preview
file_search
computer_use / computer_use_preview
mcp
code_interpreter
image_generation
local_shell
tool_search
```

Chat Completions-compatible 侧通常只能稳定接受：

```txt
function
```

即使某些 OpenAI 新模型或 Provider 支持 `custom`，第三方兼容层也不能默认支持。因此代理层默认应按最小公共子集处理。

---

### 1.3 转换层必须维护工具名映射表

如果你把 Responses 的 namespace 工具 flatten 成多个 function，模型回传的 `tool_calls[].function.name` 会变成新的扁平化名称。

因此必须保存映射：

```ts
type ToolNameMapping = {
  upstreamName: string
  sourceType: "function" | "custom_namespace" | "synthetic_builtin"
  originalName?: string
  namespace?: string
  originalTool?: unknown
}
```

示例：

```json
{
  "multi_agent_v1_spawn_agent": {
    "sourceType": "custom_namespace",
    "namespace": "multi_agent_v1",
    "originalName": "spawn_agent"
  }
}
```

这样上游返回：

```json
{
  "type": "function",
  "function": {
    "name": "multi_agent_v1_spawn_agent",
    "arguments": "{\"message\":\"analyze project\"}"
  }
}
```

可以还原为 Responses/Codex 原始工具调用语义：

```txt
custom namespace: multi_agent_v1
tool: spawn_agent
args: {"message":"analyze project"}
```

---

## 2. Responses → Chat Completions 请求转换总览

### 2.1 顶层字段映射

| Responses 字段 | Chat Completions 字段 | 处理方式 |
|---|---|---|
| `model` | `model` | 直接映射或根据渠道模型表改写 |
| `instructions` | `messages[0].role = system/developer` | 优先转为 `system`，目标支持 `developer` 时可保留 |
| `input` | `messages` | 文本、多轮消息、工具结果需要展开 |
| `tools` | `tools` | 只输出 `type:function` |
| `tool_choice` | `tool_choice` | 需要按上游能力降级 |
| `max_output_tokens` | `max_tokens` / `max_completion_tokens` | 按目标 Provider 字段支持转换 |
| `temperature` | `temperature` | 直接映射，注意模型限制 |
| `top_p` | `top_p` | 直接映射 |
| `stream` | `stream` | 直接映射，但事件格式需要转换 |
| `parallel_tool_calls` | `parallel_tool_calls` | 上游支持才传，否则丢弃 |
| `text.format` | `response_format` | JSON schema/structured outputs 需降级 |
| `reasoning` | 不一定支持 | 上游支持才传，否则丢弃或映射到 provider-specific 字段 |
| `previous_response_id` | 无直接等价 | 在 bridge 层展开为历史 messages |
| `store` | 无直接等价 | 通常丢弃 |
| `include` | 无直接等价 | 通常丢弃或由 bridge 自己填充 |

---

## 3. `tools` 转换规则

### 3.1 总体规则

```ts
function convertResponsesToolsToChatTools(tools: ResponsesTool[]): {
  chatTools: ChatCompletionTool[]
  mapping: Record<string, ToolNameMapping>
} {
  const chatTools = []
  const mapping = {}

  for (const tool of tools ?? []) {
    switch (tool.type) {
      case "function":
        addFunctionTool(tool)
        break

      case "custom":
        handleCustomTool(tool)
        break

      default:
        handleBuiltinOrUnknownTool(tool)
        break
    }
  }

  return {
    chatTools: dedupeTools(chatTools),
    mapping
  }
}
```

---

## 4. `function` 工具转换

### 4.1 Responses function 工具形态

Responses function 工具常见形态可能是：

```json
{
  "type": "function",
  "name": "get_weather",
  "description": "Get weather",
  "parameters": {
    "type": "object",
    "properties": {
      "city": {
        "type": "string"
      }
    },
    "required": ["city"]
  },
  "strict": true
}
```

而 Chat Completions 常见形态是：

```json
{
  "type": "function",
  "function": {
    "name": "get_weather",
    "description": "Get weather",
    "parameters": {
      "type": "object",
      "properties": {
        "city": {
          "type": "string"
        }
      },
      "required": ["city"]
    },
    "strict": true
  }
}
```

### 4.2 转换方式

```ts
function normalizeFunctionTool(tool: any): ChatCompletionTool {
  const functionObject = tool.function ?? tool

  return {
    type: "function",
    function: {
      name: sanitizeFunctionName(functionObject.name),
      description: functionObject.description ?? "",
      parameters: normalizeJsonSchema(
        functionObject.parameters ?? {
          type: "object",
          properties: {}
        }
      ),
      ...(functionObject.strict !== undefined
        ? { strict: functionObject.strict }
        : {})
    }
  }
}
```

### 4.3 注意事项

- `name` 必须符合目标 Provider 允许的字符集；
- 尽量使用 `[a-zA-Z0-9_-]`；
- 避免 `.`、`/`、`:`、空格；
- `parameters` 缺失时补 `{ "type": "object", "properties": {} }`；
- 如果目标 Provider 不支持 `strict`，需要丢弃；
- 如果目标 Provider 不支持完整 JSON Schema，需要降级，例如去掉 `$schema`、`oneOf`、`anyOf`、`patternProperties` 等。

---

## 5. `custom` 工具转换

Responses / Codex 场景里，`custom` 至少可能有两类：

```txt
A. namespace custom tool
B. ability/builtin declaration custom tool
```

两类不能用同一种方式处理。

---

### 5.1 namespace 型 custom：展开成多个 function

输入示例：

```json
{
  "type": "custom",
  "function": {
    "name": ""
  },
  "custom": {
    "description": "Tools for spawning and managing sub-agents.",
    "name": "multi_agent_v1",
    "type": "namespace",
    "tools": [
      {
        "type": "function",
        "name": "spawn_agent",
        "description": "Spawn a sub-agent",
        "parameters": {
          "type": "object",
          "properties": {
            "message": {
              "type": "string"
            }
          }
        }
      }
    ]
  }
}
```

输出到 Chat Completions：

```json
{
  "type": "function",
  "function": {
    "name": "multi_agent_v1_spawn_agent",
    "description": "Spawn a sub-agent",
    "parameters": {
      "type": "object",
      "properties": {
        "message": {
          "type": "string"
        }
      }
    }
  }
}
```

映射表：

```json
{
  "multi_agent_v1_spawn_agent": {
    "sourceType": "custom_namespace",
    "namespace": "multi_agent_v1",
    "originalName": "spawn_agent"
  }
}
```

推荐实现：

```ts
function flattenCustomNamespaceTool(tool: any) {
  const namespace = sanitizeFunctionName(tool.custom?.name ?? "namespace")
  const children = tool.custom?.tools ?? []

  return children
    .filter((child: any) => child.type === "function")
    .map((child: any) => {
      const originalName = child.name ?? child.function?.name
      const upstreamName = sanitizeFunctionName(`${namespace}_${originalName}`)

      return {
        chatTool: {
          type: "function",
          function: {
            name: upstreamName,
            description: child.description ?? child.function?.description ?? "",
            parameters: normalizeJsonSchema(
              child.parameters ??
              child.function?.parameters ??
              { type: "object", properties: {} }
            ),
            ...(child.strict !== undefined ? { strict: child.strict } : {})
          }
        },
        mapping: {
          upstreamName,
          sourceType: "custom_namespace",
          namespace,
          originalName,
          originalTool: child
        }
      }
    })
}
```

---

### 5.2 ability/builtin declaration 型 custom：不要直接展开

示例：

```json
{
  "type": "custom",
  "function": {
    "name": ""
  },
  "custom": {
    "external_web_access": false,
    "type": "web_search"
  }
}
```

这类不是函数集合，而是某种运行时能力声明。它没有 `custom.tools[]`，不能直接 flatten。

处理方式：

| custom 类型 | 推荐处理 |
|---|---|
| `custom.type = "web_search"` | 如果 bridge 有搜索执行器，则合成为 `web_search` function；否则丢弃 |
| `custom.type = "file_search"` | 如果 bridge 有 RAG/file search，则合成为 function；否则丢弃 |
| `custom.type = "namespace"` | 展开 `custom.tools[]` |
| 其他 custom | 包装成 `{ input: string }` function 或丢弃 |

#### web_search 的安全处理

如果出现：

```json
{
  "custom": {
    "type": "web_search",
    "external_web_access": false
  }
}
```

建议默认丢弃：

```ts
if (
  tool.type === "custom" &&
  tool.custom?.type === "web_search" &&
  tool.custom?.external_web_access !== true
) {
  return []
}
```

只有当 bridge 本身拥有搜索能力，并且策略允许搜索时，才合成为 function：

```json
{
  "type": "function",
  "function": {
    "name": "web_search",
    "description": "Search the web for fresh or external information.",
    "parameters": {
      "type": "object",
      "properties": {
        "query": {
          "type": "string",
          "description": "Search query"
        }
      },
      "required": ["query"],
      "additionalProperties": false
    }
  }
}
```

---

## 6. Responses built-in tools 转换

### 6.1 映射表

| Responses tool | 能否直接发给 Chat | 推荐处理 |
|---|---:|---|
| `function` | 是 | 转成 Chat `type:function` |
| `custom` namespace | 否 | flatten 成多个 function |
| `custom` free-form | 否 | 包装成 `{ input: string }` function 或丢弃 |
| `web_search` | 否 | bridge 实现搜索 function 或丢弃 |
| `web_search_preview` | 否 | bridge 实现搜索 function 或丢弃 |
| `file_search` | 否 | bridge 实现 RAG/file_search function 或丢弃 |
| `mcp` | 否 | 连接 MCP 后把 MCP tools 展平成 function |
| `code_interpreter` | 否 | bridge 实现沙盒执行 function 或丢弃 |
| `computer_use` | 否 | 需要完整 computer-use loop，不建议简单转 |
| `computer_use_preview` | 否 | 需要完整 computer-use loop，不建议简单转 |
| `image_generation` | 否 | 走单独图片生成管线，不建议伪装成 Chat function |
| `local_shell` | 否 | bridge 实现 shell function，且必须有权限控制 |
| `tool_search` | 否 | bridge 实现工具检索，动态注入 function |

---

### 6.2 MCP 转换

Responses 的 MCP 工具不能直接传给 Chat Completions。

推荐处理：

```txt
1. bridge 连接 MCP server
2. 拉取 MCP tool list
3. 每个 MCP tool 转成 Chat function
4. function name 加 MCP server 前缀
5. 模型调用后，bridge 还原并调用 MCP
```

示例：

```txt
mcp server: github
tool: create_issue

Chat function name:
mcp_github_create_issue
```

映射表：

```json
{
  "mcp_github_create_issue": {
    "sourceType": "synthetic_builtin",
    "provider": "mcp",
    "server": "github",
    "originalName": "create_issue"
  }
}
```

---

### 6.3 file_search 转换

如果 bridge 有文件检索能力，可合成为：

```json
{
  "type": "function",
  "function": {
    "name": "file_search",
    "description": "Search indexed user files and return relevant passages.",
    "parameters": {
      "type": "object",
      "properties": {
        "query": {
          "type": "string"
        },
        "limit": {
          "type": "integer",
          "minimum": 1,
          "maximum": 20
        }
      },
      "required": ["query"],
      "additionalProperties": false
    }
  }
}
```

工具结果返回给模型时，应包含：

```txt
- 命中文档标题
- 摘要片段
- 文件 id / citation metadata
- chunk id / line range，如果有
```

如果 bridge 没有文件检索能力，则丢弃 `file_search`，不要伪装。

---

### 6.4 code_interpreter / local_shell 转换

这类工具风险较高，只有当 bridge 真的有沙盒执行和权限控制时才暴露。

推荐 function：

```json
{
  "type": "function",
  "function": {
    "name": "exec_command",
    "description": "Run a command in a restricted sandbox.",
    "parameters": {
      "type": "object",
      "properties": {
        "cmd": {
          "type": "string"
        },
        "workdir": {
          "type": "string"
        }
      },
      "required": ["cmd"],
      "additionalProperties": false
    }
  }
}
```

不要把任意 shell 暴露给不可信模型，必须有：

```txt
- sandbox
- allowlist / denylist
- approval policy
- timeout
- output truncation
- audit log
```

---

## 7. function name 规范化

### 7.1 推荐规则

```ts
function sanitizeFunctionName(name: string): string {
  return name
    .trim()
    .replace(/[^a-zA-Z0-9_-]/g, "_")
    .replace(/_+/g, "_")
    .replace(/^_+|_+$/g, "")
    .slice(0, 64) || "tool"
}
```

### 7.2 命名冲突处理

如果多个工具规范化后重名：

```txt
search
search
```

应添加稳定后缀：

```txt
search
search_2
search_3
```

映射表必须记录最终上游名称。

```ts
function dedupeToolName(baseName: string, used: Set<string>): string {
  let name = baseName
  let i = 2
  while (used.has(name)) {
    name = `${baseName}_${i++}`
  }
  used.add(name)
  return name
}
```

---

## 8. tool_choice 转换

### 8.1 Responses tool_choice 可能形态

常见：

```json
"tool_choice": "auto"
```

```json
"tool_choice": "none"
```

```json
"tool_choice": "required"
```

```json
{
  "type": "function",
  "name": "get_weather"
}
```

```json
{
  "type": "custom",
  "name": "multi_agent_v1"
}
```

### 8.2 转换规则

| Responses `tool_choice` | Chat `tool_choice` |
|---|---|
| `auto` | `auto` |
| `none` | `none` |
| `required` | `required`，若上游不支持则转 `auto` + system 提示 |
| `{type:"function", name}` | `{type:"function", function:{name:mappedName}}` |
| `{type:"custom", name}` namespace | 如果 namespace 只有一个 child，可强制该 child；否则降级为 `auto` |
| 内置工具强制调用 | 如果 bridge 合成了 function，则强制对应 function；否则报错或降级 |

### 8.3 namespace 强制调用的特殊情况

如果用户指定：

```json
{
  "type": "custom",
  "name": "multi_agent_v1"
}
```

而 `multi_agent_v1` 展开后有 5 个 function，Chat Completions 不能强制调用一个 namespace。

可选处理：

```txt
A. 降级为 auto
B. 如果有明确 child name，强制 child
C. 加 system 提示：“You may use one of these multi_agent_v1_* tools”
```

不要随便强制第一个工具。

---

## 9. Chat Completions → Responses 输出转换

### 9.1 普通文本响应

Chat response：

```json
{
  "choices": [
    {
      "message": {
        "role": "assistant",
        "content": "Hello"
      },
      "finish_reason": "stop"
    }
  ]
}
```

Responses output：

```json
{
  "output": [
    {
      "type": "message",
      "role": "assistant",
      "content": [
        {
          "type": "output_text",
          "text": "Hello"
        }
      ]
    }
  ],
  "output_text": "Hello",
  "status": "completed"
}
```

---

### 9.2 tool_calls 转 Responses function_call

Chat response：

```json
{
  "message": {
    "role": "assistant",
    "content": null,
    "tool_calls": [
      {
        "id": "call_abc",
        "type": "function",
        "function": {
          "name": "multi_agent_v1_spawn_agent",
          "arguments": "{\"message\":\"analyze project\"}"
        }
      }
    ]
  },
  "finish_reason": "tool_calls"
}
```

Responses output：

```json
{
  "output": [
    {
      "type": "function_call",
      "id": "call_abc",
      "call_id": "call_abc",
      "name": "multi_agent_v1_spawn_agent",
      "arguments": "{\"message\":\"analyze project\"}",
      "status": "completed"
    }
  ],
  "status": "requires_action"
}
```

如果需要还原 namespace 语义，可附加 metadata，或在内部执行层使用 mapping：

```json
{
  "internal_tool_mapping": {
    "sourceType": "custom_namespace",
    "namespace": "multi_agent_v1",
    "originalName": "spawn_agent"
  }
}
```

对外是否暴露该 metadata，取决于你的 Responses 兼容目标。

---

### 9.3 多 tool_calls

Chat 支持多个 tool calls：

```json
{
  "tool_calls": [
    { "id": "call_1", "function": { "name": "a", "arguments": "{}" } },
    { "id": "call_2", "function": { "name": "b", "arguments": "{}" } }
  ]
}
```

Responses output 应生成多个 `function_call` item：

```json
{
  "output": [
    {
      "type": "function_call",
      "call_id": "call_1",
      "name": "a",
      "arguments": "{}"
    },
    {
      "type": "function_call",
      "call_id": "call_2",
      "name": "b",
      "arguments": "{}"
    }
  ]
}
```

---

## 10. 工具结果回传：Responses input → Chat messages

### 10.1 Responses function_call_output

Responses 下一轮可能传入：

```json
{
  "type": "function_call_output",
  "call_id": "call_abc",
  "output": "result text"
}
```

应转为 Chat tool message：

```json
{
  "role": "tool",
  "tool_call_id": "call_abc",
  "content": "result text"
}
```

### 10.2 如果上游不支持 `role: tool`

部分兼容 Provider 只支持 `system/user/assistant`。兜底：

```json
{
  "role": "user",
  "content": "Tool result for call_abc:\nresult text"
}
```

但这会损失 tool-call 结构语义，只作为最后兜底。

---

## 11. Chat tool call loop 转换流程

完整循环：

```txt
Client Responses request
  ↓
Bridge converts input/messages/tools
  ↓
Upstream Chat Completions
  ↓
Chat returns assistant tool_calls
  ↓
Bridge maps tool_calls → Responses function_call output
  ↓
Client executes tool or bridge executes tool
  ↓
Tool result returned as function_call_output
  ↓
Bridge maps function_call_output → role:tool message
  ↓
Upstream Chat continues
```

如果是 bridge 自己执行工具，则流程可以内聚：

```txt
Chat returns tool_calls
  ↓
Bridge executes mapped tool
  ↓
Bridge appends tool result messages
  ↓
Bridge calls upstream again
  ↓
Return final Responses message
```

注意：如果你的目标是模拟 Responses API，是否自动执行工具取决于你的产品语义。OpenAI Responses 可以返回 tool call 让应用继续，也可以在内置工具场景由平台处理。代理层需要明确边界。

---

## 12. 流式转换

### 12.1 Chat Completions streaming 常见 chunk

```json
{
  "choices": [
    {
      "delta": {
        "content": "Hello"
      }
    }
  ]
}
```

或：

```json
{
  "choices": [
    {
      "delta": {
        "tool_calls": [
          {
            "index": 0,
            "id": "call_abc",
            "type": "function",
            "function": {
              "name": "get_weather",
              "arguments": "{\"city\""
            }
          }
        ]
      }
    }
  ]
}
```

### 12.2 Responses streaming 目标事件

建议输出：

```txt
response.created
response.output_item.added
response.output_text.delta
response.output_text.done
response.function_call_arguments.delta
response.function_call_arguments.done
response.output_item.done
response.completed
response.failed
```

### 12.3 文本 delta 映射

Chat chunk：

```json
{
  "delta": {
    "content": "Hello"
  }
}
```

Responses event：

```json
{
  "type": "response.output_text.delta",
  "delta": "Hello"
}
```

### 12.4 tool_calls delta 映射

Chat chunk 里的工具调用参数通常是分片累积的。

需要 reducer：

```ts
type PartialToolCall = {
  id?: string
  type?: string
  name?: string
  arguments: string
}
```

处理逻辑：

```ts
for (const chunk of stream) {
  for (const toolDelta of chunk.choices[0].delta.tool_calls ?? []) {
    const index = toolDelta.index
    const state = partialToolCalls[index] ?? { arguments: "" }

    if (toolDelta.id) state.id = toolDelta.id
    if (toolDelta.type) state.type = toolDelta.type
    if (toolDelta.function?.name) state.name = toolDelta.function.name
    if (toolDelta.function?.arguments) {
      state.arguments += toolDelta.function.arguments

      emit({
        type: "response.function_call_arguments.delta",
        item_id: state.id,
        delta: toolDelta.function.arguments
      })
    }

    partialToolCalls[index] = state
  }
}
```

当 `finish_reason = "tool_calls"` 时，输出：

```json
{
  "type": "response.function_call_arguments.done",
  "item_id": "call_abc",
  "arguments": "{\"city\":\"Singapore\"}"
}
```

然后：

```json
{
  "type": "response.output_item.done",
  "item": {
    "type": "function_call",
    "call_id": "call_abc",
    "name": "get_weather",
    "arguments": "{\"city\":\"Singapore\"}"
  }
}
```

最后：

```json
{
  "type": "response.completed"
}
```

或如果需要应用继续提供工具结果：

```json
{
  "type": "response.completed",
  "response": {
    "status": "requires_action"
  }
}
```

---

## 13. strict / schema 兼容

### 13.1 `strict` 字段

Responses/OpenAI 可能支持：

```json
{
  "strict": true
}
```

第三方 Provider 可能不支持。建议能力开关：

```ts
if (!target.supportsStrictToolSchema) {
  delete tool.function.strict
}
```

### 13.2 JSON Schema 降级

有些 Provider 只支持 JSON Schema 子集。建议清理：

```txt
删除或降级：
- $schema
- $id
- definitions / $defs
- oneOf
- anyOf
- allOf
- not
- patternProperties
- unevaluatedProperties
- dependentSchemas
```

对 `additionalProperties: false`，通常可以保留；如果 Provider 报错，再按渠道配置删除。

---

## 14. 参数和字段过滤

发往 Chat Completions 前建议过滤 Responses-only 字段：

```txt
previous_response_id
store
include
metadata
truncation
reasoning
text
text.format
conversation
background
max_output_tokens
```

其中部分字段可转换：

```txt
max_output_tokens → max_tokens / max_completion_tokens
text.format → response_format
reasoning → provider-specific reasoning field
```

不能转换的不要原样透传。

---

## 15. 推荐目标能力配置

建议每个上游 Provider/模型配置 capability：

```ts
type ChatTargetCapabilities = {
  supportsDeveloperRole: boolean
  supportsSystemRole: boolean
  supportsToolRole: boolean
  supportsTools: boolean
  supportsToolChoiceRequired: boolean
  supportsParallelToolCalls: boolean
  supportsStrictToolSchema: boolean
  supportsResponseFormat: boolean
  supportsCustomToolType: boolean
  supportsStreamingToolCalls: boolean
}
```

默认保守配置：

```ts
const conservativeChatCapabilities = {
  supportsDeveloperRole: false,
  supportsSystemRole: true,
  supportsToolRole: true,
  supportsTools: true,
  supportsToolChoiceRequired: false,
  supportsParallelToolCalls: false,
  supportsStrictToolSchema: false,
  supportsResponseFormat: false,
  supportsCustomToolType: false,
  supportsStreamingToolCalls: true
}
```

DeepSeek / 多数 OpenAI-compatible 可从保守配置开始。

---

## 16. 推荐转换伪代码

### 16.1 请求转换

```ts
function responsesToChatRequest(req: ResponsesRequest, target: Target) {
  const toolState = convertResponsesToolsToChatTools(req.tools ?? [], target)

  return {
    chatRequest: compact({
      model: mapModel(req.model, target),
      messages: convertResponsesInputToChatMessages(req, target),
      tools: toolState.chatTools.length ? toolState.chatTools : undefined,
      tool_choice: convertToolChoice(req.tool_choice, toolState.mapping, target),
      temperature: req.temperature,
      top_p: req.top_p,
      stream: req.stream,
      max_tokens: req.max_output_tokens,
      parallel_tool_calls: target.supportsParallelToolCalls
        ? req.parallel_tool_calls
        : undefined,
      response_format: target.supportsResponseFormat
        ? convertTextFormat(req.text?.format)
        : undefined
    }),
    toolMapping: toolState.mapping
  }
}
```

### 16.2 工具转换

```ts
function convertResponsesToolsToChatTools(tools: any[], target: Target) {
  const result = []
  const mapping = {}
  const usedNames = new Set<string>()

  for (const tool of tools) {
    if (tool.type === "function") {
      const chatTool = normalizeFunctionTool(tool, target)
      chatTool.function.name = dedupeToolName(
        sanitizeFunctionName(chatTool.function.name),
        usedNames
      )

      result.push(chatTool)
      mapping[chatTool.function.name] = {
        sourceType: "function",
        originalName: tool.name ?? tool.function?.name,
        originalTool: tool
      }
      continue
    }

    if (tool.type === "custom" && tool.custom?.type === "namespace") {
      for (const flattened of flattenCustomNamespaceTool(tool, target)) {
        flattened.chatTool.function.name = dedupeToolName(
          sanitizeFunctionName(flattened.chatTool.function.name),
          usedNames
        )

        result.push(flattened.chatTool)
        mapping[flattened.chatTool.function.name] = flattened.mapping
      }
      continue
    }

    const synthetic = convertBuiltinToolToSyntheticFunction(tool, target)
    if (synthetic) {
      synthetic.chatTool.function.name = dedupeToolName(
        sanitizeFunctionName(synthetic.chatTool.function.name),
        usedNames
      )

      result.push(synthetic.chatTool)
      mapping[synthetic.chatTool.function.name] = synthetic.mapping
    }
  }

  return { chatTools: result, mapping }
}
```

### 16.3 回传转换

```ts
function chatResponseToResponses(chatResp: ChatCompletion, toolMapping: ToolMapping) {
  const choice = chatResp.choices[0]
  const msg = choice.message

  const output = []

  if (msg.content) {
    output.push({
      type: "message",
      role: "assistant",
      content: [
        {
          type: "output_text",
          text: msg.content
        }
      ]
    })
  }

  for (const call of msg.tool_calls ?? []) {
    const name = call.function.name
    const mapped = toolMapping[name]

    output.push({
      type: "function_call",
      id: call.id,
      call_id: call.id,
      name,
      arguments: call.function.arguments,
      status: "completed",
      metadata: mapped
        ? {
            source_type: mapped.sourceType,
            namespace: mapped.namespace,
            original_name: mapped.originalName
          }
        : undefined
    })
  }

  return {
    object: "response",
    status: msg.tool_calls?.length ? "requires_action" : "completed",
    output,
    output_text: output
      .flatMap(item => item.content ?? [])
      .filter(part => part.type === "output_text")
      .map(part => part.text)
      .join("")
  }
}
```

---

## 17. 错误处理建议

### 17.1 上游报 `unknown variant custom`

原因：

```txt
把 Responses custom tool 原样发给 Chat Completions 上游
```

修复：

```txt
custom namespace → flatten
custom builtin declaration → synthetic function 或丢弃
其他非 function → 丢弃
```

### 17.2 上游报 tool name invalid

原因：

```txt
function name 包含 `.`, `/`, 空格，或长度超限
```

修复：

```txt
sanitizeFunctionName + mapping
```

### 17.3 上游报 schema invalid

原因：

```txt
parameters 使用了上游不支持的 JSON Schema 特性
```

修复：

```txt
normalizeJsonSchema 按 provider capability 降级
```

### 17.4 模型调用了 bridge 无法执行的工具

原因：

```txt
把未实现的 builtin tool 伪装成 function
```

修复：

```txt
只有存在 executor 时才暴露 synthetic function
否则丢弃
```

---

## 18. 针对你给的 Codex tools 示例的处理

输入里已有普通 function tools：

```txt
exec_command
write_stdin
update_plan
get_goal
create_goal
update_goal
request_user_input
view_image
```

这些可以直接保留为 Chat function tools。

输入里还有：

```json
{
  "type": "custom",
  "custom": {
    "name": "multi_agent_v1",
    "type": "namespace",
    "tools": [...]
  }
}
```

应展开为：

```txt
multi_agent_v1_close_agent
multi_agent_v1_resume_agent
multi_agent_v1_send_input
multi_agent_v1_spawn_agent
multi_agent_v1_wait_agent
```

输入里还有：

```json
{
  "type": "custom",
  "custom": {
    "external_web_access": false,
    "type": "web_search"
  }
}
```

建议默认丢弃，因为：

```txt
它不是 namespace；
没有 custom.tools[]；
external_web_access = false；
Chat Completions 上游不认识这个 custom 类型。
```

最终发往 DeepSeek 这类上游的工具应该全是：

```txt
type = function
```

---

## 19. 最小实现清单

必须实现：

```txt
- tools whitelist：只输出 type:function
- function tool schema normalization
- custom namespace flatten
- function name sanitize + dedupe
- tool name mapping table
- Chat tool_calls → Responses function_call
- Responses function_call_output → Chat role:tool
- stream reducer：聚合 tool_call arguments
```

建议实现：

```txt
- provider/model capabilities
- JSON Schema 降级
- tool_choice 降级
- built-in tools synthetic function executor
- MCP tool discovery + flatten
- web/file search bridge executor
- strict/parallel_tool_calls/response_format 能力开关
```

不要做：

```txt
- 不要把 Responses tools 原样透传给 Chat Completions
- 不要默认暴露无法执行的 synthetic tools
- 不要用带点号的 function name
- 不要丢失 flattened name 到原始工具的映射
- 不要假设所有 Provider 都支持 tool role、strict schema、parallel tool calls
```

---

## 20. 推荐默认策略

对多数 Chat Completions-compatible 上游：

```txt
developer → system
tools[].type != function → 不透传
custom namespace → flatten to function
web_search/file_search/MCP/code_interpreter → 有 executor 才合成 function，否则丢弃
tool_choice.required → 不支持时降级 auto
strict → 不支持时删除
parallel_tool_calls → 不支持时删除
function name → sanitize + dedupe
返回 tool_calls → 转 Responses function_call
工具结果 → role:tool message
```

这套策略可以最大限度避免上游 400，并保持 Responses API 的工具循环语义尽量可逆。
