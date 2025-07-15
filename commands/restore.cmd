aws s3api list-objects-v2 \
  --bucket backup-2002 \
  --query 'Contents[?StorageClass==`DEEP_ARCHIVE`].Key' \
  --output text \
| xargs -I {} aws s3api restore-object \
  --bucket backup-2002 \
  --key "{}" \
  --restore-request '{"Days":7,"GlacierJobParameters":{"Tier":"Bulk"}}'
