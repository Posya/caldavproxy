# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.26 AS build

WORKDIR /src

# Cache dependencies separately from source for faster rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Static, stripped binary so it runs on a scratch base.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/caldavproxy .

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot

# CA certificates are included in distroless/static, needed for HTTPS upstream.
COPY --from=build /out/caldavproxy /caldavproxy

EXPOSE 8080
USER nonroot:nonroot

ENTRYPOINT ["/caldavproxy"]
