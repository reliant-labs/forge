---
title: "LLM Integration"
description: "Using Forge with AI/LLM tools for development"
weight: 60
---

# LLM Integration

Forge is designed to be LLM-friendly, making it ideal for AI-assisted development.

## Why Forge for LLMs

1. **Proto-first** - Clear contract definitions LLMs can understand
2. **Code generation** - Automatic scaffolding from proto
3. **Consistent patterns** - Predictable structure across services
4. **Type safety** - Compile-time validation
5. **Documentation** - Proto comments become documentation

## Using with Claude Code / Cursor

Forge works seamlessly with AI coding assistants:

```bash
# Create new service via LLM
# 1. Define proto
# 2. Generate code
task proto

# 3. Implement methods
# LLM can fill in service implementation based on proto
```

## MCP Integration

Forge includes an MCP (Model Context Protocol) server for AI agents:

```json
{
  "mcpServers": {
    "forge-mcp": {
      "command": "./mcp/forge-mcp/forge-mcp"
    }
  }
}
```

Available tools:
- `taskfile` - Execute development tasks
- `add_breakpoint` - Debug service methods
- `stream_logs` - Query application logs
- `query_db` - Inspect database
- And more...

## Prompting Patterns

### Creating a Service

```
Create a new UserService with methods for CRUD operations on users.
Users should have: id, email, name, created_at.
Use forge-orm (included at pkg/orm/) for database integration.
```

### Adding Features

```
Add authentication middleware to UserService that validates JWT tokens.
Extract user ID from token and add to context.
```

### Testing

```
Write comprehensive tests for UserService including:
- Unit tests with mocks
- Integration tests with test database
- Table-driven validation tests
```

## Best Practices for AI Development

1. **Start with proto** - Let LLM generate proto definitions first
2. **Generate code** - Use `task proto` before implementing
3. **Follow templates** - Reference existing services as examples
4. **Test-driven** - Have LLM write tests first
5. **Incremental** - Build features one at a time
6. **Review generated code** - Always validate LLM output

## See Also

- [Creating Services]({{< ref "creating-services" >}})
- [Proto Conventions]({{< ref "../architecture/proto-conventions" >}})