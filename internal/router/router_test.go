package router

import "testing"

func TestDetectService(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		want string
	}{
		{
			name: "dynamodb por X-Amz-Target",
			req:  Request{Target: "DynamoDB_20120810.PutItem"},
			want: "dynamodb",
		},
		{
			name: "dynamodbstreams gana sobre dynamodb por target",
			req:  Request{Target: "DynamoDBStreams_20120810.GetRecords"},
			want: "dynamodbstreams",
		},
		{
			name: "dynamodbstreams gana sobre dynamodb por host (orden de evaluación)",
			req:  Request{Host: "streams.dynamodb.us-east-1.amazonaws.com"},
			want: "dynamodbstreams",
		},
		{
			name: "sqs por Action en query",
			req:  Request{Action: "SendMessage"},
			want: "sqs",
		},
		{
			name: "sqs por host",
			req:  Request{Host: "sqs.us-east-1.amazonaws.com"},
			want: "sqs",
		},
		{
			name: "iam por Action",
			req:  Request{Action: "CreateRole"},
			want: "iam",
		},
		{
			name: "sts por Action",
			req:  Request{Action: "GetCallerIdentity"},
			want: "sts",
		},
		{
			name: "sts por credential scope",
			req:  Request{Authorization: "AWS4-HMAC-SHA256 Credential=AKIA.../20260624/us-east-1/sts/aws4_request, ..."},
			want: "sts",
		},
		{
			name: "s3 por host virtual-hosted",
			req:  Request{Host: "mybucket.s3.amazonaws.com"},
			want: "s3",
		},
		{
			name: "s3 como fallback final sin ninguna señal",
			req:  Request{Path: "/mybucket/mykey"},
			want: "s3",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectService(tc.req); got != tc.want {
				t.Errorf("DetectService(%+v) = %q, want %q", tc.req, got, tc.want)
			}
		})
	}
}

func TestAccessKeyIDFromAuthorization(t *testing.T) {
	auth := "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLE/20260624/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc"
	if got := AccessKeyIDFromAuthorization(auth); got != "AKIAEXAMPLE" {
		t.Errorf("AccessKeyIDFromAuthorization() = %q, want AKIAEXAMPLE", got)
	}
	if got := AccessKeyIDFromAuthorization(""); got != "" {
		t.Errorf("AccessKeyIDFromAuthorization(\"\") = %q, want empty", got)
	}
}
