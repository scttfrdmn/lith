#!/usr/bin/env python3
# R4 query: project id,confidence,basic_category; filter basic_category=='restaurant'.
# usage: pyquery.py local <mount-file-path> | pyquery.py s3 <bucket/key>
import sys, time
import pyarrow.dataset as ds
import pyarrow.compute as pc
import pyarrow.fs as fs

mode, target = sys.argv[1], sys.argv[2]
cols = ["id", "confidence", "basic_category"]
filt = pc.field("basic_category") == "restaurant"
if mode == "s3":
    fsys = fs.S3FileSystem(anonymous=True, region="us-west-2")
    dataset = ds.dataset(target, filesystem=fsys, format="parquet")
else:
    dataset = ds.dataset(target, format="parquet")
t0 = time.time()
tab = dataset.to_table(columns=cols, filter=filt)
print(f"{time.time()-t0:.2f} {tab.num_rows}")
