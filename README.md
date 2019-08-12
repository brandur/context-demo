# context-demo

```
brew install envrc postgres
```

## Server

``` sh
createdb context-demo
psql context-demo < schema/schema.sql

# must be run in this directory for Go module resolution to
# work
cd server
cp .envrc.sample .envrc
direnv allow
go run main.go
```

## Client

``` sh
cd client
go run main.go
```
