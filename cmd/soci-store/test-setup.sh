#!/bin/bash
# Test script for soci-store setup and functionality
# This script helps verify that soci-store is working correctly

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

SOCI_STORE_MOUNT="/var/lib/soci-store"
TEST_IMAGE="docker.io/library/alpine:latest"

echo "=== soci-store Setup Test ==="
echo

# Function to print colored status
print_status() {
    if [ $1 -eq 0 ]; then
        echo -e "${GREEN}✓${NC} $2"
    else
        echo -e "${RED}✗${NC} $2"
    fi
}

print_warning() {
    echo -e "${YELLOW}⚠${NC} $1"
}

# Check 1: Is soci-store binary built?
echo "1. Checking soci-store binary..."
if [ -f "../../out/soci-store" ]; then
    print_status 0 "soci-store binary found"
else
    print_status 1 "soci-store binary not found - run 'make soci-store' first"
    exit 1
fi

# Check 2: Is soci binary built?
echo "2. Checking soci CLI binary..."
if [ -f "../../out/soci" ]; then
    print_status 0 "soci CLI binary found"
else
    print_status 1 "soci CLI binary not found - run 'make soci' first"
    exit 1
fi

# Check 3: Is FUSE available?
echo "3. Checking FUSE support..."
if [ -e /dev/fuse ]; then
    print_status 0 "FUSE device available"
else
    print_status 1 "FUSE device not available - install fuse: sudo apt-get install fuse"
    exit 1
fi

# Check 4: Is soci-store mounted?
echo "4. Checking soci-store mount..."
if mount | grep -q "$SOCI_STORE_MOUNT"; then
    print_status 0 "soci-store is mounted at $SOCI_STORE_MOUNT"
    SOCI_RUNNING=true
else
    print_status 1 "soci-store is NOT mounted"
    print_warning "Start it with: sudo ../../out/soci-store --log-level debug $SOCI_STORE_MOUNT"
    SOCI_RUNNING=false
fi

# Check 5: Storage configuration
echo "5. Checking Podman storage configuration..."
if grep -q "additionallayerstores.*soci-store" /etc/containers/storage.conf 2>/dev/null; then
    print_status 0 "Podman storage configured for soci-store"
else
    print_status 1 "Podman storage NOT configured"
    print_warning "Add to /etc/containers/storage.conf:"
    echo '    [storage.options]'
    echo '    additionallayerstores = ["/var/lib/soci-store:ref"]'
fi

# Check 6: Pull test image
echo "6. Pulling test image: $TEST_IMAGE..."
if podman pull $TEST_IMAGE > /dev/null 2>&1; then
    print_status 0 "Successfully pulled $TEST_IMAGE"
else
    print_status 1 "Failed to pull $TEST_IMAGE"
    exit 1
fi

# Check 7: Create SOCI index
echo "7. Creating SOCI index for test image..."
if sudo ../../out/soci create $TEST_IMAGE > /dev/null 2>&1; then
    print_status 0 "SOCI index created successfully"
else
    print_status 1 "Failed to create SOCI index"
    exit 1
fi

# Check 8: Verify SOCI index
echo "8. Verifying SOCI index..."
if sudo ../../out/soci index list | grep -q alpine; then
    print_status 0 "SOCI index verified"
    echo "   Index info:"
    sudo ../../out/soci index info $TEST_IMAGE 2>/dev/null | head -10 | sed 's/^/   /'
else
    print_status 1 "SOCI index not found in list"
fi

# Check 9: Verify manifest annotation
echo "9. Checking SOCI annotation in manifest..."
if podman inspect $TEST_IMAGE 2>/dev/null | grep -q "soci.*index"; then
    print_status 0 "SOCI index annotation found in manifest"
    SOCI_DIGEST=$(podman inspect $TEST_IMAGE 2>/dev/null | grep -o '"com.amazon.soci.index-digest":"[^"]*"' | cut -d'"' -f4)
    echo "   SOCI index digest: $SOCI_DIGEST"
else
    print_status 1 "No SOCI annotation in manifest"
fi

# Check 10: Verify artifact store
echo "10. Checking artifact store content..."
if [ -d "$SOCI_STORE_MOUNT/content/blobs/sha256" ]; then
    BLOB_COUNT=$(ls "$SOCI_STORE_MOUNT/content/blobs/sha256/" 2>/dev/null | wc -l)
    if [ $BLOB_COUNT -gt 0 ]; then
        print_status 0 "Artifact store has $BLOB_COUNT blobs"
    else
        print_warning "Artifact store is empty - SOCI indexes may not be accessible"
    fi
else
    print_warning "Artifact store directory not found"
fi

# Final test: Try to run container
echo
echo "=== Final Test: Running Container ==="
if [ "$SOCI_RUNNING" = true ]; then
    echo "Attempting to run container with soci-store..."
    if timeout 10 podman run --rm $TEST_IMAGE echo "soci-store test successful!" > /tmp/soci-test.log 2>&1; then
        print_status 0 "Container ran successfully with soci-store!"
        cat /tmp/soci-test.log
    else
        print_status 1 "Container failed to run"
        echo "Error log:"
        cat /tmp/soci-test.log | tail -20
        echo
        print_warning "Check soci-store logs with: sudo journalctl -f | grep soci-store"
        print_warning "Or run soci-store in foreground with debug: sudo ../../out/soci-store --log-level debug $SOCI_STORE_MOUNT"
    fi
else
    print_warning "Cannot test container - soci-store is not running"
    echo
    echo "To run soci-store:"
    echo "  sudo ../../out/soci-store --log-level debug $SOCI_STORE_MOUNT"
fi

echo
echo "=== Summary ==="
echo "If all checks passed, soci-store is ready to use!"
echo
echo "To use with your own images:"
echo "  1. Pull the image: podman pull <your-image>"
echo "  2. Create SOCI index: sudo ../../out/soci create <your-image>"
echo "  3. Run container: podman run <your-image>"
echo
echo "For troubleshooting, see: TROUBLESHOOTING.md"
