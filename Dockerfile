# Multi-stage build for the Go shopping-site sample.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM scratch
COPY --from=build /out/server /server
# The server runs as a plain scratch-compatible binary. CA roots are not needed
# (no outbound TLS in this sample), but keep the entrypoint trivial.
EXPOSE 8080
ENTRYPOINT ["/server"]