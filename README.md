# context-demo

## Server

``` sh
createdb context-demo
psql context-demo < schema/schema.sql

# must be run in this directory for Go module resolution to
# work
cd server
go run main.go
```
