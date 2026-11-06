# Mock supplier service. Build from the repository root:
#   docker build -f examples/mock-supplier/Dockerfile -t gpuai-oss-mock-supplier .
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY examples/mock-supplier ./examples/mock-supplier
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/mock-supplier ./examples/mock-supplier

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/mock-supplier /mock-supplier
EXPOSE 8080
ENTRYPOINT ["/mock-supplier"]
