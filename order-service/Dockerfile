FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /order-service .

FROM alpine:3.20
COPY --from=build /order-service /order-service
EXPOSE 8080
ENTRYPOINT ["/order-service"]
