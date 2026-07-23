# syntax=docker/dockerfile:1

# --- build stage ---
FROM golang:1.25-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Stamp the version from the VERSION file into the binary.
# VERSION is expected to contain a MAJOR.MINOR string.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X main.version=$(cat VERSION | tr -d '[:space:]')" \
        -o /out/p2p-signal .

# --- runtime stage ---
FROM scratch

COPY --from=build /out/p2p-signal /p2p-signal

# scratch has no passwd; run as an arbitrary non-root UID.
USER 65532:65532

EXPOSE 4000

ENTRYPOINT ["/p2p-signal"]
CMD ["--addr", ":4000"]
