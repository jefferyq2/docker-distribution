// Package s3 provides a storagedriver.StorageDriver implementation to
// store blobs in Amazon S3 cloud storage.
//
// This package leverages the official aws client library for interfacing with
// S3.
//
// Because S3 is a key, value store the Stat call does not support last modification
// time for directories (directories are an abstraction for key, value stores)
//
// Keep in mind that S3 guarantees only read-after-write consistency for new
// objects, but no read-after-update or list-after-write consistency.
package s3

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"

	dcontext "github.com/docker/distribution/context"
	"github.com/docker/distribution/registry/client/transport"
	storagedriver "github.com/docker/distribution/registry/storage/driver"
	"github.com/docker/distribution/registry/storage/driver/base"
	"github.com/docker/distribution/registry/storage/driver/factory"
)

const driverName = "s3aws"

// minChunkSize defines the minimum multipart upload chunk size
// S3 API requires multipart upload chunks to be at least 5MB
const minChunkSize = 5 << 20

// maxChunkSize defines the maximum multipart upload chunk size allowed by S3.
const maxChunkSize = 5 << 30

const defaultChunkSize = 2 * minChunkSize

const (
	// defaultMultipartCopyChunkSize defines the default chunk size for all
	// but the last Upload Part - Copy operation of a multipart copy.
	// Empirically, 32 MB is optimal.
	defaultMultipartCopyChunkSize = 32 << 20

	// defaultMultipartCopyMaxConcurrency defines the default maximum number
	// of concurrent Upload Part - Copy operations for a multipart copy.
	defaultMultipartCopyMaxConcurrency = 100

	// defaultMultipartCopyThresholdSize defines the default object size
	// above which multipart copy will be used. (PUT Object - Copy is used
	// for objects at or below this size.)  Empirically, 32 MB is optimal.
	defaultMultipartCopyThresholdSize = 32 << 20
)

// listMax is the largest amount of objects you can request from S3 in a list call
const listMax = 1000

// noStorageClass defines the value to be used if storage class is not supported by the S3 endpoint
const noStorageClass = "NONE"

// validRegions maps known s3 region identifiers to region descriptors
var validRegions = map[string]struct{}{}

// validObjectACLs contains known s3 object Acls
var validObjectACLs = map[string]struct{}{}

//DriverParameters A struct that encapsulates all of the driver parameters after all values have been set
type DriverParameters struct {
	// S3 is an optional parameter. If specified, it will use the existing session
	// to construct the Driver.
	S3 *s3.S3

	AccessKey                   string
	SecretKey                   string
	Bucket                      string
	Region                      string
	RegionEndpoint              string
	Encrypt                     bool
	KeyID                       string
	Secure                      bool
	SkipVerify                  bool
	V4Auth                      bool
	ChunkSize                   int64
	MultipartCopyChunkSize      int64
	MultipartCopyMaxConcurrency int64
	MultipartCopyThresholdSize  int64
	RootDirectory               string
	StorageClass                string
	UserAgent                   string
	ObjectACL                   string
	SessionToken                string
	LogS3APIRequests            bool
	LogS3APIResponseHeaders     map[string]string
}

func init() {
	partitions := endpoints.DefaultPartitions()
	for _, p := range partitions {
		for region := range p.Regions() {
			validRegions[region] = struct{}{}
		}
	}

	for _, objectACL := range []string{
		s3.ObjectCannedACLPrivate,
		s3.ObjectCannedACLPublicRead,
		s3.ObjectCannedACLPublicReadWrite,
		s3.ObjectCannedACLAuthenticatedRead,
		s3.ObjectCannedACLAwsExecRead,
		s3.ObjectCannedACLBucketOwnerRead,
		s3.ObjectCannedACLBucketOwnerFullControl,
	} {
		validObjectACLs[objectACL] = struct{}{}
	}

	// Register this as the default s3 driver in addition to s3aws
	factory.Register("s3", &s3DriverFactory{})
	factory.Register(driverName, &s3DriverFactory{})
}

// s3DriverFactory implements the factory.StorageDriverFactory interface
type s3DriverFactory struct{}

func (factory *s3DriverFactory) Create(parameters map[string]interface{}) (storagedriver.StorageDriver, error) {
	return FromParameters(parameters)
}

type driver struct {
	S3                          *s3.S3
	Bucket                      string
	ChunkSize                   int64
	Encrypt                     bool
	KeyID                       string
	MultipartCopyChunkSize      int64
	MultipartCopyMaxConcurrency int64
	MultipartCopyThresholdSize  int64
	RootDirectory               string
	StorageClass                string
	ObjectACL                   string
	LogS3APIRequests            bool
	LogS3APIResponseHeaders     map[string]string
}

type baseEmbed struct {
	base.Base
}

// Driver is a storagedriver.StorageDriver implementation backed by Amazon S3
// Objects are stored at absolute keys in the provided bucket.
type Driver struct {
	baseEmbed
}

// FromParameters constructs a new Driver with a given parameters map
// Required parameters:
// - accesskey
// - secretkey
// - region
// - bucket
// - encrypt
func FromParameters(parameters map[string]interface{}) (*Driver, error) {
	// Providing no values for these is valid in case the user is authenticating
	// with an IAM on an ec2 instance (in which case the instance credentials will
	// be summoned when GetAuth is called)
	accessKey := parameters["accesskey"]
	if accessKey == nil {
		accessKey = ""
	}
	secretKey := parameters["secretkey"]
	if secretKey == nil {
		secretKey = ""
	}

	regionEndpoint := parameters["regionendpoint"]
	if regionEndpoint == nil {
		regionEndpoint = ""
	}

	regionName := parameters["region"]
	if regionName == nil || fmt.Sprint(regionName) == "" {
		return nil, fmt.Errorf("no region parameter provided")
	}
	region := fmt.Sprint(regionName)
	// Don't check the region value if a custom endpoint is provided.
	if regionEndpoint == "" {
		if _, ok := validRegions[region]; !ok {
			return nil, fmt.Errorf("invalid region provided: %v", region)
		}
	}

	bucket := parameters["bucket"]
	if bucket == nil || fmt.Sprint(bucket) == "" {
		return nil, fmt.Errorf("no bucket parameter provided")
	}

	encryptBool := false
	encrypt := parameters["encrypt"]
	switch encrypt := encrypt.(type) {
	case string:
		b, err := strconv.ParseBool(encrypt)
		if err != nil {
			return nil, fmt.Errorf("the encrypt parameter should be a boolean")
		}
		encryptBool = b
	case bool:
		encryptBool = encrypt
	case nil:
		// do nothing
	default:
		return nil, fmt.Errorf("the encrypt parameter should be a boolean")
	}

	secureBool := true
	secure := parameters["secure"]
	switch secure := secure.(type) {
	case string:
		b, err := strconv.ParseBool(secure)
		if err != nil {
			return nil, fmt.Errorf("the secure parameter should be a boolean")
		}
		secureBool = b
	case bool:
		secureBool = secure
	case nil:
		// do nothing
	default:
		return nil, fmt.Errorf("the secure parameter should be a boolean")
	}

	skipVerifyBool := false
	skipVerify := parameters["skipverify"]
	switch skipVerify := skipVerify.(type) {
	case string:
		b, err := strconv.ParseBool(skipVerify)
		if err != nil {
			return nil, fmt.Errorf("the skipVerify parameter should be a boolean")
		}
		skipVerifyBool = b
	case bool:
		skipVerifyBool = skipVerify
	case nil:
		// do nothing
	default:
		return nil, fmt.Errorf("the skipVerify parameter should be a boolean")
	}

	v4Bool := true
	v4auth := parameters["v4auth"]
	switch v4auth := v4auth.(type) {
	case string:
		b, err := strconv.ParseBool(v4auth)
		if err != nil {
			return nil, fmt.Errorf("the v4auth parameter should be a boolean")
		}
		v4Bool = b
	case bool:
		v4Bool = v4auth
	case nil:
		// do nothing
	default:
		return nil, fmt.Errorf("the v4auth parameter should be a boolean")
	}

	keyID := parameters["keyid"]
	if keyID == nil {
		keyID = ""
	}

	chunkSize, err := getParameterAsInt64(parameters, "chunksize", defaultChunkSize, minChunkSize, maxChunkSize)
	if err != nil {
		return nil, err
	}

	multipartCopyChunkSize, err := getParameterAsInt64(parameters, "multipartcopychunksize", defaultMultipartCopyChunkSize, minChunkSize, maxChunkSize)
	if err != nil {
		return nil, err
	}

	multipartCopyMaxConcurrency, err := getParameterAsInt64(parameters, "multipartcopymaxconcurrency", defaultMultipartCopyMaxConcurrency, 1, math.MaxInt64)
	if err != nil {
		return nil, err
	}

	multipartCopyThresholdSize, err := getParameterAsInt64(parameters, "multipartcopythresholdsize", defaultMultipartCopyThresholdSize, 0, maxChunkSize)
	if err != nil {
		return nil, err
	}

	rootDirectory := parameters["rootdirectory"]
	if rootDirectory == nil {
		rootDirectory = ""
	}

	storageClass := s3.StorageClassStandard
	storageClassParam := parameters["storageclass"]
	if storageClassParam != nil {
		storageClassString, ok := storageClassParam.(string)
		if !ok {
			return nil, fmt.Errorf("the storageclass parameter must be one of %v, %v invalid",
				[]string{s3.StorageClassStandard, s3.StorageClassReducedRedundancy}, storageClassParam)
		}
		// All valid storage class parameters are UPPERCASE, so be a bit more flexible here
		storageClassString = strings.ToUpper(storageClassString)
		if storageClassString != noStorageClass &&
			storageClassString != s3.StorageClassStandard &&
			storageClassString != s3.StorageClassReducedRedundancy {
			return nil, fmt.Errorf("the storageclass parameter must be one of %v, %v invalid",
				[]string{noStorageClass, s3.StorageClassStandard, s3.StorageClassReducedRedundancy}, storageClassParam)
		}
		storageClass = storageClassString
	}

	userAgent := parameters["useragent"]
	if userAgent == nil {
		userAgent = ""
	}

	logS3APIRequestsBool := false
	logS3APIRequests := parameters["logs3apirequests"]
	switch logS3APIRequests := logS3APIRequests.(type) {
	case string:
		b, err := strconv.ParseBool(logS3APIRequests)
		if err != nil {
			return nil, fmt.Errorf("the logS3APIRequests parameter should be a boolean")
		}
		logS3APIRequestsBool = b
	case bool:
		logS3APIRequestsBool = logS3APIRequests
	case nil:
		// do nothing
	default:
		return nil, fmt.Errorf("the logS3APIRequests parameter should be a boolean")
	}

	logS3APIResponseHeadersMap := map[string]string{}
	logS3APIResponseHeaders := parameters["logs3apiresponseheaders"]
	switch logS3APIResponseHeaders := logS3APIResponseHeaders.(type) {
	case map[string]string:
		logS3APIResponseHeadersMap = logS3APIResponseHeaders
	case nil:
		// do nothing
	default:
		return nil, fmt.Errorf("the logS3APIResponseHeaders parameter should be a map[string]string")
	}

	objectACL := s3.ObjectCannedACLPrivate
	objectACLParam := parameters["objectacl"]
	if objectACLParam != nil {
		objectACLString, ok := objectACLParam.(string)
		if !ok {
			return nil, fmt.Errorf("invalid value for objectacl parameter: %v", objectACLParam)
		}

		if _, ok = validObjectACLs[objectACLString]; !ok {
			return nil, fmt.Errorf("invalid value for objectacl parameter: %v", objectACLParam)
		}
		objectACL = objectACLString
	}

	sessionToken := ""

	params := DriverParameters{
		nil,
		fmt.Sprint(accessKey),
		fmt.Sprint(secretKey),
		fmt.Sprint(bucket),
		region,
		fmt.Sprint(regionEndpoint),
		encryptBool,
		fmt.Sprint(keyID),
		secureBool,
		skipVerifyBool,
		v4Bool,
		chunkSize,
		multipartCopyChunkSize,
		multipartCopyMaxConcurrency,
		multipartCopyThresholdSize,
		fmt.Sprint(rootDirectory),
		storageClass,
		fmt.Sprint(userAgent),
		objectACL,
		fmt.Sprint(sessionToken),
		logS3APIRequestsBool,
		logS3APIResponseHeadersMap,
	}

	return New(params)
}

// getParameterAsInt64 converts parameters[name] to an int64 value (using
// defaultt if nil), verifies it is no smaller than min, and returns it.
func getParameterAsInt64(parameters map[string]interface{}, name string, defaultt int64, min int64, max int64) (int64, error) {
	rv := defaultt
	param := parameters[name]
	switch v := param.(type) {
	case string:
		vv, err := strconv.ParseInt(v, 0, 64)
		if err != nil {
			return 0, fmt.Errorf("%s parameter must be an integer, %v invalid", name, param)
		}
		rv = vv
	case int64:
		rv = v
	case int, uint, int32, uint32, uint64:
		rv = reflect.ValueOf(v).Convert(reflect.TypeOf(rv)).Int()
	case nil:
		// do nothing
	default:
		return 0, fmt.Errorf("invalid value for %s: %#v", name, param)
	}

	if rv < min || rv > max {
		return 0, fmt.Errorf("the %s %#v parameter should be a number between %d and %d (inclusive)", name, rv, min, max)
	}

	return rv, nil
}

// New constructs a new Driver with the given AWS credentials, region, encryption flag, and
// bucketName
func New(params DriverParameters) (*Driver, error) {
	s3obj := params.S3
	if s3obj == nil {
		if !params.V4Auth &&
			(params.RegionEndpoint == "" ||
				strings.Contains(params.RegionEndpoint, "s3.amazonaws.com")) {
			return nil, fmt.Errorf("on Amazon S3 this storage driver can only be used with v4 authentication")
		}

		awsConfig := aws.NewConfig()
		sess, err := session.NewSession()
		if err != nil {
			return nil, fmt.Errorf("failed to create new session: %v", err)
		}
		creds := credentials.NewChainCredentials([]credentials.Provider{
			&credentials.StaticProvider{
				Value: credentials.Value{
					AccessKeyID:     params.AccessKey,
					SecretAccessKey: params.SecretKey,
					SessionToken:    params.SessionToken,
				},
			},
			&credentials.EnvProvider{},
			&credentials.SharedCredentialsProvider{},
			&ec2rolecreds.EC2RoleProvider{Client: ec2metadata.New(sess)},
		})

		if params.RegionEndpoint != "" {
			awsConfig.WithS3ForcePathStyle(true)
			awsConfig.WithEndpoint(params.RegionEndpoint)
		}

		awsConfig.WithCredentials(creds)
		awsConfig.WithRegion(params.Region)
		awsConfig.WithDisableSSL(!params.Secure)

		if params.UserAgent != "" || params.SkipVerify {
			httpTransport := http.DefaultTransport
			if params.SkipVerify {
				httpTransport = &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				}
			}
			if params.UserAgent != "" {
				awsConfig.WithHTTPClient(&http.Client{
					Transport: transport.NewTransport(httpTransport, transport.NewHeaderRequestModifier(http.Header{http.CanonicalHeaderKey("User-Agent"): []string{params.UserAgent}})),
				})
			} else {
				awsConfig.WithHTTPClient(&http.Client{
					Transport: transport.NewTransport(httpTransport),
				})
			}
		}

		sess, err = session.NewSession(awsConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create new session with aws config: %v", err)
		}
		s3obj = s3.New(sess)

		// enable S3 compatible signature v2 signing instead
		if !params.V4Auth {
			setv2Handlers(s3obj)
		}
	}

	// TODO Currently multipart uploads have no timestamps, so this would be unwise
	// if you initiated a new s3driver while another one is running on the same bucket.
	// multis, _, err := bucket.ListMulti("", "")
	// if err != nil {
	// 	return nil, err
	// }

	// for _, multi := range multis {
	// 	err := multi.Abort()
	// 	//TODO appropriate to do this error checking?
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// }

	d := &driver{
		S3:                          s3obj,
		Bucket:                      params.Bucket,
		ChunkSize:                   params.ChunkSize,
		Encrypt:                     params.Encrypt,
		KeyID:                       params.KeyID,
		MultipartCopyChunkSize:      params.MultipartCopyChunkSize,
		MultipartCopyMaxConcurrency: params.MultipartCopyMaxConcurrency,
		MultipartCopyThresholdSize:  params.MultipartCopyThresholdSize,
		RootDirectory:               params.RootDirectory,
		StorageClass:                params.StorageClass,
		ObjectACL:                   params.ObjectACL,
		LogS3APIRequests:            params.LogS3APIRequests,
		LogS3APIResponseHeaders:     params.LogS3APIResponseHeaders,
	}

	return &Driver{
		baseEmbed: baseEmbed{
			Base: base.Base{
				StorageDriver: d,
			},
		},
	}, nil
}

// logS3OperationHandlerName is used to identify the handler used to log S3 API
// requests
const logS3OperationHandlerName = "docker.storage-driver.s3.operation-logger"

// logS3Operation logs each S3 operation, including request and response info,
// as it completes
func (d *driver) logS3Operation(ctx context.Context) request.NamedHandler {
	return request.NamedHandler{
		Name: logS3OperationHandlerName,
		Fn: func(r *request.Request) {
			req := r.HTTPRequest
			resp := r.HTTPResponse
			duration := time.Now().Sub(r.Time)
			op := r.Operation
			fields := map[interface{}]interface{}{
				"s3_operation_name":                        op.Name,
				"s3_bucket_name":                           d.Bucket,
				"s3_object_name":                           d.s3Path(req.URL.Query().Get("Key")),
				"s3_http_request_method":                   req.Method,
				"s3_http_request_host":                     req.Host,
				"s3_http_request_path":                     req.URL.Path,
				"s3_http_request_content-length":           req.ContentLength,
				"s3_http_request_remote-addr":              req.RemoteAddr,
				"s3_http_response_header_x-amz-request-id": resp.Header.Values("x-amz-request-id"),
				"s3_http_response_status":                  resp.StatusCode,
				"s3_http_response_content-length":          resp.ContentLength,
				"s3_http_request_duration":                 duration.Seconds(),
				"s3_http_request_attempted_time":           r.AttemptTime,
				"s3_http_request_retry_count":              r.RetryCount,
				"s3_http_request_time":                     r.Time.Unix(),
			}

			for logKey, headerKey := range d.LogS3APIResponseHeaders {
				values := resp.Header.Values(headerKey)
				if len(values) == 1 {
					fields[logKey] = values[0]
					continue
				}
				if len(values) > 0 {
					fields[logKey] = values
				}
			}

			ll := dcontext.GetLoggerWithFields(ctx, fields)
			ll.Info("S3 operation completed")
		},
	}
}

// setContentLengthHandlerHame is used to identify the handler used set the
// ContentLength field on request data output types that support it.
const setContentLengthHandlerName = "docker.storage-driver.s3.set-content-length"

// setContentLength is used to set the ContentLength field on request data
// output types that support it.
var setContentLength = request.NamedHandler{
	Name: setContentLengthHandlerName,
	Fn: func(r *request.Request) {
		switch v := r.Data.(type) {
		case *s3.HeadObjectOutput:
			if r.HTTPResponse.ContentLength > 0 {
				v.SetContentLength(r.HTTPResponse.ContentLength)
			}
		case *s3.GetObjectOutput:
			if r.HTTPResponse.ContentLength > 0 {
				v.SetContentLength(r.HTTPResponse.ContentLength)
			}
		}
	},
}

func (d *driver) s3Client(ctx context.Context) *s3.S3 {
	s := d.S3

	if d.LogS3APIRequests {
		s = &s3.S3{Client: client.New(d.S3.Client.Config, d.S3.Client.ClientInfo, d.S3.Client.Handlers.Copy())}
		r := d.logS3Operation(ctx)
		s.Client.Handlers.Complete.PushBackNamed(r)
	}
	s.Client.Handlers.Complete.PushBackNamed(setContentLength)

	return s
}

// Implement the storagedriver.StorageDriver interface

func (d *driver) Name() string {
	return driverName
}

// GetContent retrieves the content stored at "path" as a []byte.
func (d *driver) GetContent(ctx context.Context, path string) ([]byte, error) {
	reader, err := d.Reader(ctx, path, 0)
	if err != nil {
		return nil, err
	}
	return ioutil.ReadAll(reader)
}

// PutContent stores the []byte content at a location designated by "path".
func (d *driver) PutContent(ctx context.Context, path string, contents []byte) error {
	s := d.s3Client(ctx)

	_, err := s.PutObject(&s3.PutObjectInput{
		Bucket:               aws.String(d.Bucket),
		Key:                  aws.String(d.s3Path(path)),
		ContentType:          d.getContentType(),
		ACL:                  d.getACL(),
		ServerSideEncryption: d.getEncryptionMode(),
		SSEKMSKeyId:          d.getSSEKMSKeyID(),
		StorageClass:         d.getStorageClass(),
		Body:                 bytes.NewReader(contents),
	})
	return parseError(path, err)
}

// Reader retrieves an io.ReadCloser for the content stored at "path" with a
// given byte offset.
func (d *driver) Reader(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	s := d.s3Client(ctx)

	resp, err := s.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(d.Bucket),
		Key:    aws.String(d.s3Path(path)),
		Range:  aws.String("bytes=" + strconv.FormatInt(offset, 10) + "-"),
	})

	if err != nil {
		if s3Err, ok := err.(awserr.Error); ok && s3Err.Code() == "InvalidRange" {
			return ioutil.NopCloser(bytes.NewReader(nil)), nil
		}

		return nil, parseError(path, err)
	}
	return resp.Body, nil
}

// Writer returns a FileWriter which will store the content written to it
// at the location designated by "path" after the call to Commit.
func (d *driver) Writer(ctx context.Context, path string, append bool) (storagedriver.FileWriter, error) {
	s := d.s3Client(ctx)

	key := d.s3Path(path)
	if !append {
		// TODO (brianbland): cancel other uploads at this path
		resp, err := s.CreateMultipartUpload(&s3.CreateMultipartUploadInput{
			Bucket:               aws.String(d.Bucket),
			Key:                  aws.String(key),
			ContentType:          d.getContentType(),
			ACL:                  d.getACL(),
			ServerSideEncryption: d.getEncryptionMode(),
			SSEKMSKeyId:          d.getSSEKMSKeyID(),
			StorageClass:         d.getStorageClass(),
		})
		if err != nil {
			return nil, err
		}
		return d.newWriter(key, *resp.UploadId, nil), nil
	}
	resp, err := s.ListMultipartUploads(&s3.ListMultipartUploadsInput{
		Bucket: aws.String(d.Bucket),
		Prefix: aws.String(key),
	})
	if err != nil {
		return nil, parseError(path, err)
	}

	for _, multi := range resp.Uploads {
		if key != *multi.Key {
			continue
		}
		resp, err := s.ListParts(&s3.ListPartsInput{
			Bucket:   aws.String(d.Bucket),
			Key:      aws.String(key),
			UploadId: multi.UploadId,
		})
		if err != nil {
			return nil, parseError(path, err)
		}
		var multiSize int64
		for _, part := range resp.Parts {
			multiSize += *part.Size
		}
		return d.newWriter(key, *multi.UploadId, resp.Parts), nil
	}
	return nil, storagedriver.PathNotFoundError{Path: path}
}

// Stat retrieves the FileInfo for the given path, including the current size
// in bytes and the creation time.
func (d *driver) Stat(ctx context.Context, path string) (storagedriver.FileInfo, error) {
	s := d.s3Client(ctx)

	fi := storagedriver.FileInfoFields{
		Path: path,
	}

	headResp, err := s.HeadObjectWithContext(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(d.Bucket),
		Key:    aws.String(d.s3Path(path)),
	})
	if err == nil && headResp.ContentLength != nil {
		if headResp.ContentLength != nil {
			fi.Size = *headResp.ContentLength
		}
		if headResp.LastModified != nil {
			fi.ModTime = *headResp.LastModified
		}

		return storagedriver.FileInfoInternal{FileInfoFields: fi}, nil
	}

	resp, err := s.ListObjectsWithContext(ctx, &s3.ListObjectsInput{
		Bucket:  aws.String(d.Bucket),
		Prefix:  aws.String(d.s3Path(path)),
		MaxKeys: aws.Int64(1),
	})
  
	if err != nil {
		return nil, err
	}

	if len(resp.Contents) == 1 {
		if *resp.Contents[0].Key != d.s3Path(path) {
			fi.IsDir = true
		} else {
			fi.IsDir = false
			fi.Size = *resp.Contents[0].Size
			fi.ModTime = *resp.Contents[0].LastModified
		}
	} else if len(resp.CommonPrefixes) == 1 {
		fi.IsDir = true
	} else {
		return nil, storagedriver.PathNotFoundError{Path: path}
	}

	return storagedriver.FileInfoInternal{FileInfoFields: fi}, nil
}

// List returns a list of the objects that are direct descendants of the given path.
func (d *driver) List(ctx context.Context, opath string) ([]string, error) {
	s := d.s3Client(ctx)

	path := opath
	if path != "/" && path[len(path)-1] != '/' {
		path = path + "/"
	}

	// This is to cover for the cases when the rootDirectory of the driver is either "" or "/".
	// In those cases, there is no root prefix to replace and we must actually add a "/" to all
	// results in order to keep them as valid paths as recognized by storagedriver.PathRegexp
	prefix := ""
	if d.s3Path("") == "" {
		prefix = "/"
	}

	resp, err := s.ListObjects(&s3.ListObjectsInput{
		Bucket:    aws.String(d.Bucket),
		Prefix:    aws.String(d.s3Path(path)),
		Delimiter: aws.String("/"),
		MaxKeys:   aws.Int64(listMax),
	})
	if err != nil {
		return nil, parseError(opath, err)
	}

	files := []string{}
	directories := []string{}

	for {
		for _, key := range resp.Contents {
			files = append(files, strings.Replace(*key.Key, d.s3Path(""), prefix, 1))
		}

		for _, commonPrefix := range resp.CommonPrefixes {
			commonPrefix := *commonPrefix.Prefix
			directories = append(directories, strings.Replace(commonPrefix[0:len(commonPrefix)-1], d.s3Path(""), prefix, 1))
		}

		if *resp.IsTruncated {
			resp, err = s.ListObjects(&s3.ListObjectsInput{
				Bucket:    aws.String(d.Bucket),
				Prefix:    aws.String(d.s3Path(path)),
				Delimiter: aws.String("/"),
				MaxKeys:   aws.Int64(listMax),
				Marker:    resp.NextMarker,
			})
			if err != nil {
				return nil, err
			}
		} else {
			break
		}
	}

	if opath != "/" {
		if len(files) == 0 && len(directories) == 0 {
			// Treat empty response as missing directory, since we don't actually
			// have directories in s3.
			return nil, storagedriver.PathNotFoundError{Path: opath}
		}
	}

	return append(files, directories...), nil
}

// Move moves an object stored at sourcePath to destPath, removing the original
// object.
func (d *driver) Move(ctx context.Context, sourcePath string, destPath string) error {
	/* This is terrible, but aws doesn't have an actual move. */
	if err := d.copy(ctx, sourcePath, destPath); err != nil {
		return err
	}
	return d.Delete(ctx, sourcePath)
}

// copy copies an object stored at sourcePath to destPath.
func (d *driver) copy(ctx context.Context, sourcePath string, destPath string) error {
	// S3 can copy objects up to 5 GB in size with a single PUT Object - Copy
	// operation. For larger objects, the multipart upload API must be used.
	//
	// Empirically, multipart copy is fastest with 32 MB parts and is faster
	// than PUT Object - Copy for objects larger than 32 MB.

	fileInfo, err := d.Stat(ctx, sourcePath)
	if err != nil {
		return parseError(sourcePath, err)
	}

	s := d.s3Client(ctx)

	if fileInfo.Size() <= d.MultipartCopyThresholdSize {
		_, err := s.CopyObject(&s3.CopyObjectInput{
			Bucket:               aws.String(d.Bucket),
			Key:                  aws.String(d.s3Path(destPath)),
			ContentType:          d.getContentType(),
			ACL:                  d.getACL(),
			ServerSideEncryption: d.getEncryptionMode(),
			SSEKMSKeyId:          d.getSSEKMSKeyID(),
			StorageClass:         d.getStorageClass(),
			CopySource:           aws.String(d.Bucket + "/" + d.s3Path(sourcePath)),
		})
		if err != nil {
			return parseError(sourcePath, err)
		}
		return nil
	}

	createResp, err := s.CreateMultipartUpload(&s3.CreateMultipartUploadInput{
		Bucket:               aws.String(d.Bucket),
		Key:                  aws.String(d.s3Path(destPath)),
		ContentType:          d.getContentType(),
		ACL:                  d.getACL(),
		SSEKMSKeyId:          d.getSSEKMSKeyID(),
		ServerSideEncryption: d.getEncryptionMode(),
		StorageClass:         d.getStorageClass(),
	})
	if err != nil {
		return err
	}

	numParts := (fileInfo.Size() + d.MultipartCopyChunkSize - 1) / d.MultipartCopyChunkSize
	completedParts := make([]*s3.CompletedPart, numParts)
	errChan := make(chan error, numParts)
	limiter := make(chan struct{}, d.MultipartCopyMaxConcurrency)

	for i := range completedParts {
		i := int64(i)
		go func() {
			limiter <- struct{}{}
			firstByte := i * d.MultipartCopyChunkSize
			lastByte := firstByte + d.MultipartCopyChunkSize - 1
			if lastByte >= fileInfo.Size() {
				lastByte = fileInfo.Size() - 1
			}
			uploadResp, err := s.UploadPartCopy(&s3.UploadPartCopyInput{
				Bucket:          aws.String(d.Bucket),
				CopySource:      aws.String(d.Bucket + "/" + d.s3Path(sourcePath)),
				Key:             aws.String(d.s3Path(destPath)),
				PartNumber:      aws.Int64(i + 1),
				UploadId:        createResp.UploadId,
				CopySourceRange: aws.String(fmt.Sprintf("bytes=%d-%d", firstByte, lastByte)),
			})
			if err == nil {
				completedParts[i] = &s3.CompletedPart{
					ETag:       uploadResp.CopyPartResult.ETag,
					PartNumber: aws.Int64(i + 1),
				}
			}
			errChan <- err
			<-limiter
		}()
	}

	for range completedParts {
		err := <-errChan
		if err != nil {
			return err
		}
	}

	_, err = s.CompleteMultipartUpload(&s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(d.Bucket),
		Key:             aws.String(d.s3Path(destPath)),
		UploadId:        createResp.UploadId,
		MultipartUpload: &s3.CompletedMultipartUpload{Parts: completedParts},
	})
	return err
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Delete recursively deletes all objects stored at "path" and its subpaths.
// We must be careful since S3 does not guarantee read after delete consistency
func (d *driver) Delete(ctx context.Context, path string) error {
	s3Objects := make([]*s3.ObjectIdentifier, 0, listMax)

	// manually add the given path if it's a file
	stat, err := d.Stat(ctx, path)
	if err != nil {
		return err
	}
	if stat != nil && !stat.IsDir() {
		path := d.s3Path(path)
		s3Objects = append(s3Objects, &s3.ObjectIdentifier{
			Key: &path,
		})
	}

	// list objects under the given path as a subpath (suffix with slash "/")
	s3Path := d.s3Path(path) + "/"
	listObjectsInput := &s3.ListObjectsInput{
		Bucket: aws.String(d.Bucket),
		Prefix: aws.String(s3Path),
	}

	s := d.s3Client(ctx)
ListLoop:
	for {
		// list all the objects
		resp, err := s.ListObjects(listObjectsInput)

		// resp.Contents can only be empty on the first call
		// if there were no more results to return after the first call, resp.IsTruncated would have been false
		// and the loop would be exited without recalling ListObjects
		if err != nil || len(resp.Contents) == 0 {
			break ListLoop
		}

		for _, key := range resp.Contents {
			s3Objects = append(s3Objects, &s3.ObjectIdentifier{
				Key: key.Key,
			})
		}

		// resp.Contents must have at least one element or we would have returned not found
		listObjectsInput.Marker = resp.Contents[len(resp.Contents)-1].Key

		// from the s3 api docs, IsTruncated "specifies whether (true) or not (false) all of the results were returned"
		// if everything has been returned, break
		if resp.IsTruncated == nil || !*resp.IsTruncated {
			break
		}
	}

	total := len(s3Objects)
	if total == 0 {
		return storagedriver.PathNotFoundError{Path: path}
	}

	// need to chunk objects into groups of 1000 per s3 restrictions
	for i := 0; i < total; i += 1000 {
		output, err := s.DeleteObjects(&s3.DeleteObjectsInput{
			Bucket: aws.String(d.Bucket),
			Delete: &s3.Delete{
				Objects: s3Objects[i:min(i+1000, total)],
				Quiet:   aws.Bool(false),
			},
		})
		if err != nil {
			return err
		}
		if output.Errors != nil && len(output.Errors) > 0 {
			// ideally all errors would be returned in some way
			// until then, at least pass back the first error code
			oErr := output.Errors[0]
			return errors.New(*oErr.Code)
		}
	}
	return nil
}

// URLFor returns a URL which may be used to retrieve the content stored at the given path.
// May return an UnsupportedMethodErr in certain StorageDriver implementations.
func (d *driver) URLFor(ctx context.Context, path string, options map[string]interface{}) (string, error) {
	s := d.s3Client(ctx)

	methodString := "GET"
	method, ok := options["method"]
	if ok {
		methodString, ok = method.(string)
		if !ok || (methodString != "GET" && methodString != "HEAD") {
			return "", storagedriver.ErrUnsupportedMethod{}
		}
	}

	expiresIn := 20 * time.Minute
	expires, ok := options["expiry"]
	if ok {
		et, ok := expires.(time.Time)
		if ok {
			expiresIn = time.Until(et)
		}
	}

	var req *request.Request

	switch methodString {
	case "GET":
		req, _ = s.GetObjectRequest(&s3.GetObjectInput{
			Bucket: aws.String(d.Bucket),
			Key:    aws.String(d.s3Path(path)),
		})
	case "HEAD":
		req, _ = s.HeadObjectRequest(&s3.HeadObjectInput{
			Bucket: aws.String(d.Bucket),
			Key:    aws.String(d.s3Path(path)),
		})
	default:
		panic("unreachable")
	}

	return req.Presign(expiresIn)
}

// Walk traverses a filesystem defined within driver, starting
// from the given path, calling f on each file
func (d *driver) Walk(ctx context.Context, from string, f storagedriver.WalkFn) error {
	path := from
	if !strings.HasSuffix(path, "/") {
		path = path + "/"
	}

	prefix := ""
	if d.s3Path("") == "" {
		prefix = "/"
	}

	var objectCount int64
	if err := d.doWalk(ctx, &objectCount, d.s3Path(path), prefix, f); err != nil {
		return err
	}

	// S3 doesn't have the concept of empty directories, so it'll return path not found if there are no objects
	if objectCount == 0 {
		return storagedriver.PathNotFoundError{Path: from}
	}

	return nil
}

func (d *driver) doWalk(parentCtx context.Context, objectCount *int64, path, prefix string, f storagedriver.WalkFn) error {
	var (
		retError error
		// the most recent directory walked for de-duping
		prevDir string
		// the most recent skip directory to avoid walking over undesirable files
		prevSkipDir string
	)
	prevDir = prefix + path

	listObjectsInput := &s3.ListObjectsV2Input{
		Bucket:  aws.String(d.Bucket),
		Prefix:  aws.String(path),
		MaxKeys: aws.Int64(listMax),
	}

	s := d.s3Client(parentCtx)

	ctx, done := dcontext.WithTrace(parentCtx)
	defer done("s3aws.ListObjectsV2Pages(%s)", path)

	// When the "delimiter" argument is omitted, the S3 list API will list all objects in the bucket
	// recursively, omitting directory paths. Objects are listed in sorted, depth-first order so we
	// can infer all the directories by comparing each object path to the last one we saw.
	// See: https://docs.aws.amazon.com/AmazonS3/latest/userguide/ListingKeysUsingAPIs.html

	// With files returned in sorted depth-first order, directories are inferred in the same order.
	// ErrSkipDir is handled by explicitly skipping over any files under the skipped directory. This may be sub-optimal
	// for extreme edge cases but for the general use case in a registry, this is orders of magnitude
	// faster than a more explicit recursive implementation.
	listObjectErr := s.ListObjectsV2PagesWithContext(ctx, listObjectsInput, func(objects *s3.ListObjectsV2Output, lastPage bool) bool {
		walkInfos := make([]storagedriver.FileInfoInternal, 0, len(objects.Contents))

		for _, file := range objects.Contents {
			filePath := strings.Replace(*file.Key, d.s3Path(""), prefix, 1)

			// get a list of all inferred directories between the previous directory and this file
			dirs := directoryDiff(prevDir, filePath)
			if len(dirs) > 0 {
				for _, dir := range dirs {
					walkInfos = append(walkInfos, storagedriver.FileInfoInternal{
						FileInfoFields: storagedriver.FileInfoFields{
							IsDir: true,
							Path:  dir,
						},
					})
					prevDir = dir
				}
			}

			walkInfos = append(walkInfos, storagedriver.FileInfoInternal{
				FileInfoFields: storagedriver.FileInfoFields{
					IsDir:   false,
					Size:    *file.Size,
					ModTime: *file.LastModified,
					Path:    filePath,
				},
			})
		}

		for _, walkInfo := range walkInfos {
			// skip any results under the last skip directory
			if prevSkipDir != "" && strings.HasPrefix(walkInfo.Path(), prevSkipDir) {
				continue
			}

			err := f(walkInfo)
			*objectCount++

			if err != nil {
				if err == storagedriver.ErrSkipDir {
					if walkInfo.IsDir() {
						prevSkipDir = walkInfo.Path()
						continue
					}
					// is file, stop gracefully
					return false
				}
				retError = err
				return false
			}
		}
		return true
	})

	if retError != nil {
		return retError
	}

	if listObjectErr != nil {
		return listObjectErr
	}

	return nil
}

// directoryDiff finds all directories that are not in common between
// the previous and current paths in sorted order.
//
// Eg 1 directoryDiff("/path/to/folder", "/path/to/folder/folder/file")
//   => [ "/path/to/folder/folder" ],
// Eg 2 directoryDiff("/path/to/folder/folder1", "/path/to/folder/folder2/file")
//   => [ "/path/to/folder/folder2" ]
// Eg 3 directoryDiff("/path/to/folder/folder1/file", "/path/to/folder/folder2/file")
//  => [ "/path/to/folder/folder2" ]
// Eg 4 directoryDiff("/path/to/folder/folder1/file", "/path/to/folder/folder2/folder1/file")
//   => [ "/path/to/folder/folder2", "/path/to/folder/folder2/folder1" ]
// Eg 5 directoryDiff("/", "/path/to/folder/folder/file")
//   => [ "/path", "/path/to", "/path/to/folder", "/path/to/folder/folder" ],
func directoryDiff(prev, current string) []string {
	var paths []string

	if prev == "" || current == "" {
		return paths
	}

	parent := current
	for {
		parent = filepath.Dir(parent)
		if parent == "/" || parent == prev || strings.HasPrefix(prev, parent) {
			break
		}
		paths = append(paths, parent)
	}
	reverse(paths)
	return paths
}

func reverse(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

func (d *driver) s3Path(path string) string {
	return strings.TrimLeft(strings.TrimRight(d.RootDirectory, "/")+path, "/")
}

// S3BucketKey returns the s3 bucket key for the given storage driver path.
func (d *Driver) S3BucketKey(path string) string {
	return d.StorageDriver.(*driver).s3Path(path)
}

func parseError(path string, err error) error {
	if s3Err, ok := err.(awserr.Error); ok {
		switch s3Err.Code() {
		case "NoSuchKey":
			return storagedriver.PathNotFoundError{Path: path}
		case "QuotaExceeded":
			return storagedriver.QuotaExceededError{}
		}
	}

	return err
}

func (d *driver) getEncryptionMode() *string {
	if !d.Encrypt {
		return nil
	}
	if d.KeyID == "" {
		return aws.String("AES256")
	}
	return aws.String("aws:kms")
}

func (d *driver) getSSEKMSKeyID() *string {
	if d.KeyID != "" {
		return aws.String(d.KeyID)
	}
	return nil
}

func (d *driver) getContentType() *string {
	return aws.String("application/octet-stream")
}

func (d *driver) getACL() *string {
	return aws.String(d.ObjectACL)
}

func (d *driver) getStorageClass() *string {
	if d.StorageClass == noStorageClass {
		return nil
	}
	return aws.String(d.StorageClass)
}

// writer attempts to upload parts to S3 in a buffered fashion where the last
// part is at least as large as the chunksize, so the multipart upload could be
// cleanly resumed in the future. This is violated if Close is called after less
// than a full chunk is written.
type writer struct {
	driver      *driver
	key         string
	uploadID    string
	parts       []*s3.Part
	size        int64
	readyPart   []byte
	pendingPart []byte
	closed      bool
	committed   bool
	cancelled   bool
}

func (d *driver) newWriter(key, uploadID string, parts []*s3.Part) storagedriver.FileWriter {
	var size int64
	for _, part := range parts {
		size += *part.Size
	}
	return &writer{
		driver:   d,
		key:      key,
		uploadID: uploadID,
		parts:    parts,
		size:     size,
	}
}

type completedParts []*s3.CompletedPart

func (a completedParts) Len() int           { return len(a) }
func (a completedParts) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a completedParts) Less(i, j int) bool { return *a[i].PartNumber < *a[j].PartNumber }

func (w *writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("already closed")
	} else if w.committed {
		return 0, fmt.Errorf("already committed")
	} else if w.cancelled {
		return 0, fmt.Errorf("already cancelled")
	}

	// If the last written part is smaller than minChunkSize, we need to make a
	// new multipart upload :sadface:
	if len(w.parts) > 0 && int(*w.parts[len(w.parts)-1].Size) < minChunkSize {
		var completedUploadedParts completedParts
		for _, part := range w.parts {
			completedUploadedParts = append(completedUploadedParts, &s3.CompletedPart{
				ETag:       part.ETag,
				PartNumber: part.PartNumber,
			})
		}

		sort.Sort(completedUploadedParts)

		_, err := w.driver.S3.CompleteMultipartUpload(&s3.CompleteMultipartUploadInput{
			Bucket:   aws.String(w.driver.Bucket),
			Key:      aws.String(w.key),
			UploadId: aws.String(w.uploadID),
			MultipartUpload: &s3.CompletedMultipartUpload{
				Parts: completedUploadedParts,
			},
		})
		if err != nil {
			w.driver.S3.AbortMultipartUpload(&s3.AbortMultipartUploadInput{
				Bucket:   aws.String(w.driver.Bucket),
				Key:      aws.String(w.key),
				UploadId: aws.String(w.uploadID),
			})
			return 0, parseError(w.key, err)
		}

		resp, err := w.driver.S3.CreateMultipartUpload(&s3.CreateMultipartUploadInput{
			Bucket:               aws.String(w.driver.Bucket),
			Key:                  aws.String(w.key),
			ContentType:          w.driver.getContentType(),
			ACL:                  w.driver.getACL(),
			ServerSideEncryption: w.driver.getEncryptionMode(),
			StorageClass:         w.driver.getStorageClass(),
		})
		if err != nil {
			return 0, parseError(w.key, err)
		}
		w.uploadID = *resp.UploadId

		// If the entire written file is smaller than minChunkSize, we need to make
		// a new part from scratch :double sad face:
		if w.size < minChunkSize {
			resp, err := w.driver.S3.GetObject(&s3.GetObjectInput{
				Bucket: aws.String(w.driver.Bucket),
				Key:    aws.String(w.key),
			})
			if err != nil {
				return 0, parseError(w.key, err)
			}
			defer resp.Body.Close()
			w.parts = nil
			w.readyPart, err = ioutil.ReadAll(resp.Body)
			if err != nil {
				return 0, err
			}
		} else {
			// Otherwise we can use the old file as the new first part
			copyPartResp, err := w.driver.S3.UploadPartCopy(&s3.UploadPartCopyInput{
				Bucket:     aws.String(w.driver.Bucket),
				CopySource: aws.String(w.driver.Bucket + "/" + w.key),
				Key:        aws.String(w.key),
				PartNumber: aws.Int64(1),
				UploadId:   resp.UploadId,
			})
			if err != nil {
				return 0, parseError(w.key, err)
			}
			w.parts = []*s3.Part{
				{
					ETag:       copyPartResp.CopyPartResult.ETag,
					PartNumber: aws.Int64(1),
					Size:       aws.Int64(w.size),
				},
			}
		}
	}

	var n int

	for len(p) > 0 {
		// If no parts are ready to write, fill up the first part
		if neededBytes := int(w.driver.ChunkSize) - len(w.readyPart); neededBytes > 0 {
			if len(p) >= neededBytes {
				w.readyPart = append(w.readyPart, p[:neededBytes]...)
				n += neededBytes
				p = p[neededBytes:]
			} else {
				w.readyPart = append(w.readyPart, p...)
				n += len(p)
				p = nil
			}
		}

		if neededBytes := int(w.driver.ChunkSize) - len(w.pendingPart); neededBytes > 0 {
			if len(p) >= neededBytes {
				w.pendingPart = append(w.pendingPart, p[:neededBytes]...)
				n += neededBytes
				p = p[neededBytes:]
				err := w.flushPart()
				if err != nil {
					w.size += int64(n)
					return n, err
				}
			} else {
				w.pendingPart = append(w.pendingPart, p...)
				n += len(p)
				p = nil
			}
		}
	}
	w.size += int64(n)
	return n, nil
}

func (w *writer) Size() int64 {
	return w.size
}

func (w *writer) Close() error {
	if w.closed {
		return fmt.Errorf("already closed")
	}
	w.closed = true
	return w.flushPart()
}

func (w *writer) Cancel() error {
	if w.closed {
		return fmt.Errorf("already closed")
	} else if w.committed {
		return fmt.Errorf("already committed")
	}
	w.cancelled = true
	_, err := w.driver.S3.AbortMultipartUpload(&s3.AbortMultipartUploadInput{
		Bucket:   aws.String(w.driver.Bucket),
		Key:      aws.String(w.key),
		UploadId: aws.String(w.uploadID),
	})
	return parseError(w.key, err)
}

func (w *writer) Commit() error {
	if w.closed {
		return fmt.Errorf("already closed")
	} else if w.committed {
		return fmt.Errorf("already committed")
	} else if w.cancelled {
		return fmt.Errorf("already cancelled")
	}
	err := w.flushPart()
	if err != nil {
		return err
	}
	w.committed = true

	var completedUploadedParts completedParts
	for _, part := range w.parts {
		completedUploadedParts = append(completedUploadedParts, &s3.CompletedPart{
			ETag:       part.ETag,
			PartNumber: part.PartNumber,
		})
	}

	sort.Sort(completedUploadedParts)

	_, err = w.driver.S3.CompleteMultipartUpload(&s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(w.driver.Bucket),
		Key:      aws.String(w.key),
		UploadId: aws.String(w.uploadID),
		MultipartUpload: &s3.CompletedMultipartUpload{
			Parts: completedUploadedParts,
		},
	})
	if err != nil {
		w.driver.S3.AbortMultipartUpload(&s3.AbortMultipartUploadInput{
			Bucket:   aws.String(w.driver.Bucket),
			Key:      aws.String(w.key),
			UploadId: aws.String(w.uploadID),
		})
		return parseError(w.key, err)
	}
	return nil
}

// flushPart flushes buffers to write a part to S3.
// Only called by Write (with both buffers full) and Close/Commit (always)
func (w *writer) flushPart() error {
	if len(w.readyPart) == 0 && len(w.pendingPart) == 0 {
		// nothing to write
		return nil
	}
	if len(w.pendingPart) < int(w.driver.ChunkSize) {
		// closing with a small pending part
		// combine ready and pending to avoid writing a small part
		w.readyPart = append(w.readyPart, w.pendingPart...)
		w.pendingPart = nil
	}

	partNumber := aws.Int64(int64(len(w.parts) + 1))
	resp, err := w.driver.S3.UploadPart(&s3.UploadPartInput{
		Bucket:     aws.String(w.driver.Bucket),
		Key:        aws.String(w.key),
		PartNumber: partNumber,
		UploadId:   aws.String(w.uploadID),
		Body:       bytes.NewReader(w.readyPart),
	})
	if err != nil {
		return parseError(w.key, err)
	}
	w.parts = append(w.parts, &s3.Part{
		ETag:       resp.ETag,
		PartNumber: partNumber,
		Size:       aws.Int64(int64(len(w.readyPart))),
	})
	w.readyPart = w.pendingPart
	w.pendingPart = nil
	return nil
}
