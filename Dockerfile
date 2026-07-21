FROM golang:1.23-alpine AS build
WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/photo-gallery .

FROM alpine:3.22
WORKDIR /app

COPY --from=build /out/photo-gallery /app/photo-gallery

EXPOSE 8080

ENV ADDR=:8080
ENV PHOTO_ROOT=/photos
# Persist the index cache and generated thumbnails here. Mount a writable volume
# at /cache to make them survive container recreation; otherwise they live in the
# container's ephemeral layer and are regenerated for each fresh container.
ENV GALLERY_CACHE=/cache/index.gob
ENV THUMB_CACHE=/cache/thumbs

CMD ["/app/photo-gallery"]
