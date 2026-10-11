# Copyright 2026 Abhinav Gautam
# SPDX-License-Identifier: Apache-2.0
#
# extrawest_ocpi (MIT, github.com/extrawest/extrawest_ocpi), an OCPI
# 2.2.1 implementation on FastAPI, as an interop peer. It is installed
# from PyPI at build time, not redistributed; app.py is our own script
# that runs it with an in-memory store.
# Python 3.11: the package pins pydantic 1.10.12, which fails to import
# on Python 3.12.4 and later.
FROM python:3.11.9-slim
RUN pip install --no-cache-dir extrawest-ocpi==2025.7.16 uvicorn==0.30.6
COPY app.py /peer/app.py
WORKDIR /peer
EXPOSE 8000
CMD ["python", "-u", "app.py"]
