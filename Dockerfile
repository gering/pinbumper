# syntax=docker/dockerfile:1
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=0.1.0" -o /out/pinbumper ./cmd/pinbumper

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/pinbumper /pinbumper
USER nonroot:nonroot
ENTRYPOINT ["/pinbumper"]
CMD ["--help"]
