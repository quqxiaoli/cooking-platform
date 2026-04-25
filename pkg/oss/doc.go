// Package oss wraps the Aliyun OSS SDK for image upload operations.
// presign.go — GenerateSTS: issues a short-lived STS token so the client
//              can upload directly to OSS without routing bytes through Go.
// callback.go — VerifyCallback: validates the OSS callback signature after
//               the client finishes uploading, then records the final URL.
// Added in Step 9 (image upload module).
package oss
