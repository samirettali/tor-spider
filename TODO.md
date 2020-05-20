## General
- [ ] Save all data on SIGINT
- [ ] Change all `os.Getenv` to `os.LookupEnv`

## Concurrency
- [ ] Fix `msgChan` and `errChan` sizes in order to prevent deadlock

## Storage
- [ ] Make `PageStorage` an interface
- [ ] Refactor `ElasticPageStorage`
- [ ] Store responses headers

## Collectors
- [ ] Make another collector for URLs added from the webserver, in order to be
    able to crawl clearnet and subreddits
