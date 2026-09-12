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
# server at <distDir>/standalone/server.js plus pruned node_modules. The
# static assets and public/ directory must be copied alongside it.
# Production builds use distDir ".next-prod" (next.config.ts fences them
# off from the dev server's .next), so all paths below are .next-prod.
#
# server.js sits directly under standalone/ here because the build context
# is the FRONTEND directory alone: WORKDIR /app holds the frontend, with no
# npm workspace root above it, so next.config.ts's outputFileTracingRoot walk
# finds none and roots at /app. Traced paths are then relative to /app and
# the bundle is flat.
#
# On a HOST build that root is the project's workspace root instead (that is
# what keeps the trace from writing a partial `next` into the frontend's own
# node_modules and poisoning the next build — see next.config.ts), so server.js
# lands under standalone/frontends/<name>/. Nothing consumes the host-built
# standalone tree; `forge env up` and `next start` read <distDir> directly.
# Do not "fix" that difference by re-pinning the tracing root to the frontend.
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
COPY --from=builder --chown=node:node /app/.next-prod/standalone ./
COPY --from=builder --chown=node:node /app/.next-prod/static ./.next-prod/static
COPY --from=builder --chown=node:node /app/public ./public

USER node

EXPOSE 3000

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:3000/ || exit 1

CMD ["node", "server.js"]
