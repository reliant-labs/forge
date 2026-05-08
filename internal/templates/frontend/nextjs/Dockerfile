# syntax=docker/dockerfile:1

# --- Build ---
FROM node:22-alpine AS builder
WORKDIR /app

COPY package.json package-lock.json* ./
RUN --mount=type=cache,target=/root/.npm npm install

COPY . .

ENV NEXT_TELEMETRY_DISABLED=1
ENV NODE_ENV=production

RUN npm run build

# --- Serve with Next.js standalone runtime ---
# next.config.ts sets `output: "standalone"`, which emits a minimal Node
# server at .next/standalone/server.js plus pruned node_modules. The
# static assets and public/ directory must be copied alongside it.
FROM node:22-alpine AS runner
WORKDIR /app

ENV NEXT_TELEMETRY_DISABLED=1
ENV NODE_ENV=production
ENV PORT=3000
ENV HOSTNAME=0.0.0.0

# The upstream `node` image ships a built-in non-root `node` user
# (uid=1000). Running the server as root adds no value and makes a
# container escape materially more dangerous; we drop privileges before
# CMD and own the copied assets so the process can still read them.
COPY --from=builder --chown=node:node /app/.next/standalone ./
COPY --from=builder --chown=node:node /app/.next/static ./.next/static
COPY --from=builder --chown=node:node /app/public ./public

USER node

EXPOSE 3000

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:3000/ || exit 1

CMD ["node", "server.js"]