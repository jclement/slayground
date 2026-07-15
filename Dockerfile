FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /slayground .

FROM scratch
COPY --from=build /slayground /slayground
EXPOSE 80
ENTRYPOINT ["/slayground"]
