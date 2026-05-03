# Echo Provider Example

The smallest possible Glue provider. It echoes the user's most recent
text back as the assistant's response.

This package is the runnable reference implementation for the
[Provider Plugin Guide](../../docs/provider-guide.md). Copy this layout
when building a new provider; replace the body of `stream()` with your
backend code.

## Test it

```sh
go test ./examples/echo-provider
```

Four tests cover:

- happy-path round trip through `glue.NewAgent` + `session.Prompt`
- compile-time assertion that `*Provider` satisfies `glue.Provider`
- empty transcript still emits a `Done` event (no panic, no hang)
- `Prefix` field is applied to the echoed text

No network access; nothing is gated.

## Use it as a starting point

```go
import (
    "context"

    "glue"
    echo "glue/examples/echo-provider"
)

agent := glue.NewAgent(glue.AgentOptions{Provider: echo.New()})
session, _ := agent.Session(context.Background(), "demo")
result, _ := session.Prompt(context.Background(), "ping")
fmt.Println(result.Text) // -> "ping"
```
