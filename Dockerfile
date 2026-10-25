# syntax=docker/dockerfile:1

# ---- Stage 1: build ----
# This stage has the full Go toolchain (about 1 GB). Nothing from it ships.
# Match the tag to the "go" line in your go.mod (check: head -3 go.mod).
FROM golang:1.26 AS build
WORKDIR /src

# Copy ONLY the dependency files first and download. Docker caches each
# step, so this layer is reused until go.mod/go.sum change, and rebuilds
# after code edits skip the download.
COPY go.mod go.sum ./
RUN go mod download

# Now copy the source. Editing code invalidates the cache from here down.
COPY . .

# CGO_ENABLED=0 produces a fully static binary (no C libraries needed),
# which is what lets us run it in an image with nothing else installed.
# -trimpath removes local file paths; -s -w strips debug info (smaller binary).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- Stage 2: run ----
# distroless/static has no shell and no package manager: a tiny attack
# surface. The "nonroot" variant runs as an unprivileged user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 8080

# Exec form (JSON array) makes our binary PID 1, so it receives SIGTERM
# directly. That is what triggers the graceful shutdown from Step 11.
# Shell form (ENTRYPOINT /server) would put a shell in between, and the
# signal would never reach the app.
ENTRYPOINT ["/server"]