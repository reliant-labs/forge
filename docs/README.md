# Forge Documentation

This directory contains the Hugo-based documentation for Forge.

## Quick Start

### Prerequisites

- Hugo Extended 0.100.0 or later
- Go 1.21 or later (for Hugo modules)

### Install Hugo

**macOS:**
```bash
brew install hugo
```

**Linux:**
```bash
# Download from https://gohugo.io/installation/
```

### Development Server

```bash
# Install dependencies
make setup

# Start development server with live reload
make dev
```

Visit http://localhost:1313 to view the documentation.

## Documentation Structure

```
docs/
├── content/
│   └── docs/                    # Main documentation content
│       ├── _index.md            # Homepage
│       ├── getting-started/     # Getting started guides
│       ├── architecture/        # Architecture deep-dives
│       ├── core-concepts/       # Fundamental concepts
│       ├── guides/              # Step-by-step tutorials
│       ├── cli-reference/       # CLI command reference
│       └── api-reference/       # API documentation
├── layouts/
│   ├── shortcodes/              # Custom Hugo shortcodes
│   └── partials/                # Partial templates
├── static/
│   ├── css/                     # Custom CSS
│   ├── images/                  # Images and screenshots
│   └── icons/                   # Favicons and icons
├── themes/
│   └── lotusdocs/               # Theme (git submodule)
├── hugo.toml                    # Production config
├── hugo.dev.toml                # Development config
├── Makefile                     # Build commands
└── README.md                    # This file
```

## Available Commands

```bash
make help          # Show all available commands
make setup         # Initial setup (install Hugo + init submodules)
make dev           # Start development server
make serve         # Start production server locally
make build         # Build for production
make clean         # Clean build artifacts
make preview       # Build + serve production locally
make test-build    # Quick production build test
make stats         # Show documentation statistics
make new-page      # Create a new page (PATH=docs/path/to/page)
```

## Writing Documentation

### Creating a New Page

```bash
make new-page PATH=docs/guides/my-guide
```

This creates a new file with the proper front matter template.

### Front Matter

All documentation pages should include front matter:

```yaml
---
title: "Page Title"
description: "Brief description for SEO"
weight: 20              # Controls ordering (lower = first)
icon: "icon_name"       # Material Design icon name
---
```

### Shortcodes

#### Figure

Display images with captions:

```markdown
{{< figure src="/images/screenshot.png"
           alt="Description"
           caption="Image caption"
           size="normal" >}}
```

Sizes: `normal` (default), `small`, `inline`

#### Cards

Display content in card layout:

```markdown
{{< cards >}}
  {{< card title="Card 1" icon="fas-icon" link="/docs/page1" >}}
    Card description
  {{< /card >}}
  {{< card title="Card 2" icon="fas-icon" link="/docs/page2" >}}
    Card description
  {{< /card >}}
{{< /cards >}}
```

### Internal Links

Use Hugo's `relref` for internal links:

```markdown
[Link Text]({{< relref "page-name" >}})
[Link Text]({{< relref "../other-section/page" >}})
```

### Code Blocks

Specify language for syntax highlighting:

````markdown
```go
func main() {
    fmt.Println("Hello, World!")
}
```
````

Supported languages: `go`, `proto`, `bash`, `yaml`, `json`, `toml`, etc.

## Theme

This documentation uses the [LotusDocs](https://github.com/colinwilson/lotusdocs) theme, which provides:

- Clean, professional design
- Built-in search functionality
- Dark mode support
- Responsive layout
- Mobile-friendly navigation
- Syntax highlighting with Prism
- Material Design Icons

### Updating the Theme

```bash
make update-theme
```

## Building for Production

### Local Build

```bash
make build
```

Output is in the `public/` directory.

### Production Checklist

Before deploying:

1. Run `make test-build` to ensure no errors
2. Check for broken links: `make check` (requires `html-proofer`)
3. Review the output: `make preview`
4. Check documentation stats: `make stats`

## Deployment

### GitHub Pages

The documentation can be deployed to GitHub Pages using GitHub Actions.

Create `.github/workflows/deploy.yml`:

```yaml
name: Deploy Documentation

on:
  push:
    branches: [main]

permissions:
  contents: read
  pages: write
  id-token: write

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          submodules: recursive

      - name: Setup Hugo
        uses: peaceiris/actions-hugo@v2
        with:
          hugo-version: 'latest'
          extended: true

      - name: Build
        run: |
          cd docs
          hugo --gc --minify --baseURL "${{ steps.pages.outputs.base_url }}/"

      - name: Upload artifact
        uses: actions/upload-pages-artifact@v2
        with:
          path: docs/public

  deploy:
    needs: build
    runs-on: ubuntu-latest
    environment:
      name: github-pages
      url: ${{ steps.deployment.outputs.page_url }}
    steps:
      - name: Deploy to GitHub Pages
        id: deployment
        uses: actions/deploy-pages@v2
```

### Custom Domain

1. Add `static/CNAME` with your domain
2. Configure DNS with your provider
3. Enable HTTPS in GitHub repository settings

## Configuration

### hugo.toml (Production)

Main configuration file for production builds.

### hugo.dev.toml (Development)

Development-specific configuration:
- Enables draft content
- Faster builds
- Local baseURL

### Customization

Edit these files to customize:

- `hugo.toml` - Site configuration
- `static/css/custom.css` - Custom styles
- `layouts/` - Layout overrides
- `data/` - Data files for templates

## Troubleshooting

### Hugo Not Found

Install Hugo Extended:
```bash
make install
```

### Theme Issues

Update git submodules:
```bash
git submodule update --init --recursive
make update-theme
```

### Build Errors

1. Clean and rebuild:
   ```bash
   make clean
   make build
   ```

2. Check Hugo version:
   ```bash
   hugo version
   ```
   Requires Hugo Extended 0.100.0+

### Port Already in Use

Change the port:
```bash
hugo server --port 1314
```

## Contributing

### Documentation Style Guide

1. **Use clear, concise language**
2. **Include code examples** for concepts
3. **Add screenshots** where helpful
4. **Link to related pages** using `relref`
5. **Keep pages focused** on one topic
6. **Use proper heading hierarchy** (H1 → H2 → H3)

### Testing Changes

1. Start dev server: `make dev`
2. Make your changes
3. Verify in browser (live reload)
4. Test production build: `make test-build`
5. Submit pull request

## Resources

- [Hugo Documentation](https://gohugo.io/documentation/)
- [LotusDocs Theme](https://github.com/colinwilson/lotusdocs)
- [CommonMark Spec](https://commonmark.org/)
- [Material Design Icons](https://material.io/resources/icons/)

## License

Documentation content is licensed under [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/).

Code examples in documentation are licensed under [Apache 2.0](https://www.apache.org/licenses/LICENSE-2.0).
