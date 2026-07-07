FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /static-files .

# run
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /static-files /static-files
EXPOSE 8334
USER nonroot:nonroot
ENTRYPOINT ["/static-files"]
