#!/bin/bash

container_name="audio-processor"

if [ $# -eq 0 ]; then
    # No arguments provided - run interactive shell
    docker exec -it $container_name /bin/bash
else
    # Pass all arguments to the container
    docker exec -it $container_name "$@"
fi
