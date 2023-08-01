package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/golang-jwt/jwt/v5"
	log "github.com/sirupsen/logrus"
	"net/http"
	"os"
	"time"
)

type SubmissionRequest struct {
	SubmissionName string `json:"submissionName"`
	Sha256         string `json:"sha256"`
	Notifications  []struct {
		Channel string `json:"channel"`
		Target  string `json:"target"`
	} `json:"notifications"`
}

type SubmissionResponse struct {
	Data struct {
		Attributes struct {
			AwsAccessKeyId     string `json:"awsAccessKeyId"`
			AwsSecretAccessKey string `json:"awsSecretAccessKey"`
			AwsSessionToken    string `json:"awsSessionToken"`
			Bucket             string `json:"bucket"`
			Object             string `json:"object"`
		} `json:"attributes"`
		Id   string `json:"id"`
		Type string `json:"type"`
	} `json:"data"`
}

type SubmissionStatusResponse struct {
	Data struct {
		Attributes struct {
			CreatedDate time.Time `json:"createdDate"`
			Name        string    `json:"name"`
			Status      string    `json:"status"`
		} `json:"attributes"`
		Id   string `json:"id"`
		Type string `json:"type"`
	} `json:"data"`
	Meta struct {
	} `json:"meta"`
}

func main() {

	file := flag.String("f", "", "file to notarize")
	key := flag.String("k", "", "private key")
	kid := flag.String("kid", "", "kid for jwt (required)")
	iss := flag.String("iss", "", "iss for jwt (required)")
	s3Timeout := flag.Duration("s3t", time.Minute, "aws s3 timeout to upload file")
	checkPeriod := flag.Duration("cp", time.Second*10, "period to check notarization")
	jwtOnly := flag.Bool("jwtout", false, "only output jwt")
	flag.Parse()
	if *file == "" || *key == "" || *kid == "" || *iss == "" || *s3Timeout == 0 {
		flag.PrintDefaults()
		os.Exit(-1)
	}

	if *jwtOnly {
		jwtKey, err := createJwtToken(*iss, *kid, *key)
		if err != nil {
			log.Fatal(err)
		}
		println(jwtKey)
	}

	fileStat, err := os.Stat(*file)
	if err != nil {
		log.Fatal(err)
	}
	fileName := fileStat.Name()
	hash := sha256.New()
	fileHash := fmt.Sprintf("%x", hash.Sum(nil))

	fileData, err := os.ReadFile(*file)
	if err != nil {
		log.Fatal(err)
	}
	hash.Write(fileData)

	jwtKey, err := createJwtToken(*iss, *kid, *key)
	if err != nil {
		log.Fatal(err)
	}

	resp, err := startSubmission(&SubmissionRequest{
		SubmissionName: fileName,
		Sha256:         fileHash,
	}, &jwtKey)

	if err != nil {
		log.Fatal(err)
	}

	err = uploadFile(resp, fileData, *s3Timeout)
	if err != nil {
		log.Fatal(err)
	}

	checkRespErrCounter := 0
	for {
		checkResp, err := checkSubmission(resp.Data.Id, jwtKey)
		if err != nil {
			log.Error(err)
			if checkRespErrCounter == 5 {
				log.Fatal(err)
			}
			checkRespErrCounter++
			time.Sleep(*checkPeriod)
		}

		if checkResp.Data.Attributes.Status == "Accepted" {
			log.Info("file was accepted. Notarization successfull")
			os.Exit(0)
		}

		if checkResp.Data.Attributes.Status == "In Progress" {
			log.Infof("Notarization %s in progress", checkResp.Data.Id)
			time.Sleep(*checkPeriod)
			continue
		} else {
			log.Info(checkResp)
			os.Exit(-1)
		}

	}

}

func createJwtToken(iss string, kid string, keyfile string) (string, error) {
	keyData, err := os.ReadFile(keyfile)
	if err != nil {
		return "", errors.New(fmt.Sprintf("cannot find key at \"%s\"", keyfile))
	}
	key, err := jwt.ParseECPrivateKeyFromPEM(keyData)
	if err != nil {
		return "", errors.New(fmt.Sprintf("cannot parse key from \"%s\"", keyfile))
	}
	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": iss,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Minute * 15).Unix(),
		"aud": "appstoreconnect-v1",
	})
	token.Header["kid"] = kid

	result, err := token.SignedString(key)
	if err != nil {
		return "", err
	}
	return result, nil
}

func uploadFile(response *SubmissionResponse, fileData []byte, timeout time.Duration) error {
	sess := session.Must(session.NewSession(&aws.Config{
		Credentials: credentials.NewStaticCredentials(response.Data.Attributes.AwsAccessKeyId, response.Data.Attributes.AwsSecretAccessKey, response.Data.Attributes.AwsSessionToken),
	}))

	// Create a new instance of the service's client with a Session.
	// Optional aws.Config values can also be provided as variadic arguments
	// to the New function. This option allows you to provide service
	// specific configuration.
	svc := s3.New(sess)

	// Create a context with a timeout that will abort the upload if it takes
	// more than the passed in timeout.
	ctx := context.Background()
	var cancelFn func()
	if timeout < time.Minute {
		timeout = time.Minute
	}
	ctx, cancelFn = context.WithTimeout(ctx, timeout)
	// Ensure the context is canceled to prevent leaking.
	// See context package for more information, https://golang.org/pkg/context/
	if cancelFn != nil {
		defer cancelFn()
	}

	// Uploads the object to S3. The Context will interrupt the request if the
	// timeout expires.
	_, err := svc.PutObjectWithContext(ctx, &s3.PutObjectInput{
		Bucket: aws.String(response.Data.Attributes.Bucket),
		Key:    aws.String(response.Data.Attributes.Object),
		Body:   bytes.NewReader(fileData),
	})
	if err != nil {
		if awserr, ok := err.(awserr.Error); ok && awserr.Code() == request.CanceledErrorCode {
			// If the SDK can determine the request or retry delay was canceled
			// by a context the CanceledErrorCode error code will be returned.
			return errors.New(fmt.Sprintf("upload canceled due to timeout, %v\n", awserr))
		} else {
			return errors.New(fmt.Sprintf("failed to upload object, %v\n", err))
		}
	}

	return nil
}

func startSubmission(subReq *SubmissionRequest, jwt *string) (*SubmissionResponse, error) {
	if data, err := json.Marshal(subReq); err != nil {
		return nil, err
	} else {
		reader := bytes.NewReader(data)
		request, err := http.NewRequest(http.MethodPost, "https://appstoreconnect.apple.com/notary/v2/submissions", reader)
		request.Header.Add("Authorization", "Bearer "+*jwt)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(request)
		if err != nil {
			return nil, err
		}
		response := SubmissionResponse{}
		err = json.NewDecoder(resp.Body).Decode(response)
		if err != nil {
			return nil, err
		}
		return &response, nil
	}
}

func checkSubmission(id string, jwt string) (*SubmissionStatusResponse, error) {
	newRequest, err := http.NewRequest(http.MethodGet, fmt.Sprintf("https://appstoreconnect.apple.com/notary/v2/submissions/%s", id), nil)
	newRequest.Header.Add("Authorization", "Bearer "+jwt)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(newRequest)
	if err != nil {
		return nil, err
	}
	response := SubmissionStatusResponse{}
	err = json.NewDecoder(resp.Body).Decode(response)
	if err != nil {
		return nil, err
	}
	return &response, nil
}