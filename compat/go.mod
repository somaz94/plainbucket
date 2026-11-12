module github.com/somaz94/plainbucket/compat

go 1.26

replace github.com/somaz94/plainbucket => ../

require (
	github.com/aws/aws-sdk-go-v2 v1.47.8
	github.com/aws/aws-sdk-go-v2/credentials v1.20.17
	github.com/aws/aws-sdk-go-v2/feature/s3/manager v1.23.23
	github.com/aws/aws-sdk-go-v2/service/s3 v1.114.9
	github.com/aws/aws-sdk-go-v2/service/sts v1.51.9
	github.com/aws/smithy-go v1.28.8
	github.com/somaz94/plainbucket v0.0.0
)

require (
	github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream v1.7.26 // indirect
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.5.11 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.8.11 // indirect
	github.com/aws/aws-sdk-go-v2/internal/v4a v1.5.11 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding v1.13.24 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/checksum v1.11.12 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/presigned-url v1.14.11 // indirect
	github.com/aws/aws-sdk-go-v2/service/internal/s3shared v1.20.11 // indirect
)
