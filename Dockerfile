FROM golang:1.26
WORKDIR /app
COPY . .
RUN go build -o /usr/local/bin/iparent .
EXPOSE 8097
CMD ["iparent"]
