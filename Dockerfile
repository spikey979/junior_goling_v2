FROM golang:1.21-alpine AS build
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /app/bin/aidispatcher ./cmd/app

FROM alpine:3.19
WORKDIR /app
COPY --from=build /app/bin/aidispatcher /app/aidispatcher
RUN mkdir -p /app/logs
ENV PORT=8080
CMD ["/app/aidispatcher"]
