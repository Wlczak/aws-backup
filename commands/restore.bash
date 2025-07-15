aws s3api list-objects-v2 \
    --bucket backup-2002 \
    --query 'Contents[?StorageClass==`DEEP_ARCHIVE`].Key' \
    --output json |
    jq -r '.[]' |
    while IFS= read -r key; do
        aws s3api restore-object \
            --bucket backup-2002 \
            --key "$key" \
            --restore-request '{"Days":7,"GlacierJobParameters":{"Tier":"Bulk"}}'
    done
echo "restore done"
