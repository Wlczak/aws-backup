aws s3api list-objects-v2 --bucket backup-2002 --query 'Contents[?StorageClass==`DEEP_ARCHIVE`].Key' --output text --max-items 50 |
    while IFS= read -r key; do
        #echo $key \n
        aws s3api restore-object --bucket backup-2002 --key "$key" --restore-request '{"Days":7,"GlacierJobParameters":{"Tier":"Bulk"}}'
    done
