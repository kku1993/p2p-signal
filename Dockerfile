# syntax=docker/dockerfile:1

# --- build stage ---
FROM golang:1.25-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static, stripped binary: CGO disabled so no glibc dependency.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/p2p-signal .

# --- runtime stage ---
FROM scratch

COPY --from=build /out/p2p-signal /p2p-signal

# scratch has no passwd; run as an arbitrary non-root UID.
USER 65532:65532

EXPOSE 4000

ENTRYPOINT ["/p2p-signal"]
CMD ["--addr", ":4000"]
