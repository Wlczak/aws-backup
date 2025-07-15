package main

import (
	"context"
	"log"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/joho/godotenv"
)

func setupEnv() error {
	_, err := os.Stat(".env")
	if os.IsNotExist(err) {
		f, err := os.Create(".env")
		if err != nil {
			return err
		}
		defer func() {
			if err := f.Close(); err != nil {
				log.Println(err)
			}
		}()
	}

	// it would be nice to finish this, but it desn't have priority rn

	// envMap, err := godotenv.Read(".env")

	// godotenv.Read(".env")
	// for _, value := range EnvVars {
	// 	if envMap[value] == "" {
	// 		godotenv.Write(map[string]string{
	// 			value: "",
	// 		}, ".env")
	// 	}
	// 	fmt.Println(value)
	// }

	err = godotenv.Load()

	return err
}

func main() {
	// Load the Shared AWS Configuration (~/.aws/config)
	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion("eu-north-1"))
	if err != nil {
		log.Fatal(err)
	}

	setupEnv()

	bucketName := os.Getenv("BUCKET_NAME")

	if bucketName == "" {
		log.Fatal("BUCKET_NAME must be set")
	}

	// Create an Amazon S3 service client
	client := s3.NewFromConfig(cfg)
	// Get the first page of results for ListObjectsV2 for a bucket
	// output, err := client.ListObjectsV2(context.TODO(), &s3.ListObjectsV2Input{
	// 	Bucket: aws.String(bucketName),
	// })
	// if err != nil {
	// 	log.Fatal(err)
	// }

	// log.Println("first page results")
	// for _, object := range output.Contents {
	// 	log.Printf("key=%s size=%d", aws.ToString(object.Key), *object.Size)
	// }

	file, err := os.Open("yourfile.txt")
	if err != nil {
		log.Fatalf("failed to open file: %v", err)
	}
	defer file.Close()

	out, err := client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:       aws.String(bucketName),
		Key:          aws.String("yourfile.txt"),
		Body:         file,
		StorageClass: types.StorageClassDeepArchive,
	})

	if err != nil {
		log.Fatalf("failed to upload file: %v", err)
	}

	size := out.Size

	log.Printf("file uploaded successfully, size=%d", size)
}
