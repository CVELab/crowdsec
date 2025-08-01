#!/usr/bin/env bash

# this should have effect globally, for all tests
# https://github.com/bats-core/bats-core/blob/master/docs/source/warnings/BW02.rst
bats_require_minimum_version 1.5.0

debug() {
    echo 'exec 1<&-; exec 2<&-; exec 1>&3; exec 2>&1'
}
export -f debug

# redirects stdout and stderr to &3 otherwise the errors in setup, teardown would
# go unreported.
# BUT - don't do this in test functions. Everything written to stdout and
# stderr after this line will go to the terminal, but in the tests, these
# are supposed to be collected and shown only in case of test failure
# (see options --print-output-on-failure and --show-output-of-passing-tests)
eval "$(debug)"

# Allow tests to use relative paths for helper scripts.
# shellcheck disable=SC2164
cd "${TEST_DIR}"
export PATH="${TEST_DIR}/bin:${PATH}"

# complain if there's a crowdsec running system-wide or leftover from a previous test
./bin/assert-crowdsec-not-running

# we can prepend the filename to the test descriptions (useful to feed a TAP consumer)
if [[ "${PREFIX_TEST_NAMES_WITH_FILE:-false}" == "true" ]]; then
  BATS_TEST_NAME_PREFIX="$(basename "${BATS_TEST_FILENAME}" .bats): "
  export BATS_TEST_NAME_PREFIX
fi

# before bats 1.7, we did that by hand
FILE=
export FILE

# the variables exported here can be seen in other setup/teardown/test functions
# MYVAR=something
# export MYVAR

# functions too
cscli() {
    "${CSCLI}" "$@"
}
export -f cscli

config_get() {
    local cfg="${CONFIG_YAML}"
    if [[ $# -ge 2 ]]; then
        cfg="$1"
        shift
    fi

    yq e "$1" "${cfg}"
}
export -f config_get

config_set() {
    local cfg="${CONFIG_YAML}"
    if [[ $# -ge 2 ]]; then
        cfg="$1"
        shift
    fi

    yq e "$1" -i "${cfg}"
}
export -f config_set

config_disable_agent() {
    config_set '.crowdsec_service.enable=false'
    # this should be equivalent to:
    # config_set 'del(.crowdsec_service)'
}
export -f config_disable_agent

config_log_stderr() {
    config_set '.common.log_media="stdout"'
}
export -f config_log_stderr

config_disable_lapi() {
    config_set '.api.server.enable=false'
    # this should be equivalent to:
    # config_set 'del(.api.server)'
}
export -f config_disable_lapi

config_disable_capi() {
    config_set 'del(.api.server.online_client)'
}
export -f config_disable_capi

config_enable_capi() {
    online_api_credentials="$(dirname "${CONFIG_YAML}")/online_api_credentials.yaml" \
        config_set '.api.server.online_client.credentials_path=strenv(online_api_credentials)'
}
export -f config_enable_capi

# We use these functions like this:
#    somecommand <(stderr)
# to provide a standard input to "somecommand".
# The alternatives echo "$stderr" or <<<"$stderr"
# ("here string" in bash jargon)
# are worse because they add a newline,
# even if the variable is empty.

# shellcheck disable=SC2154
stderr() {
    printf '%s' "${stderr}"
}
export -f stderr

# shellcheck disable=SC2154
output() {
    printf '%s' "${output}"
}
export -f output

is_package_testing() {
    [[ "$PACKAGE_TESTING" != "" ]]
}
export -f is_package_testing

is_db_postgres() {
    [[ "$DB_BACKEND" =~ ^postgres|pgx$ ]]
}
export -f is_db_postgres

is_db_mysql() {
    [[ "$DB_BACKEND" == "mysql" ]]
}
export -f is_db_mysql

is_db_sqlite() {
    [[ "$DB_BACKEND" == "sqlite" ]]
}
export -f is_db_sqlite

crowdsec_log() {
    echo "$(config_get .common.log_dir)"/crowdsec.log
}
export -f crowdsec_log

truncate_log() {
    true > "$(crowdsec_log)"
}
export -f truncate_log

assert_log() {
    local oldout="${output:-}"
    output="$(cat "$(crowdsec_log)")"
    assert_output "$@"
    output="${oldout}"
}
export -f assert_log

cert_serial_number() {
    cfssl certinfo -cert "$1" | jq -r '.serial_number'
}
export -f cert_serial_number

# Compare ignoring the key order, and allow "expected" without quoted identifiers.
# Preserve the output variable in case the following commands require it.
assert_json() {
    local oldout="${output}"
    # validate actual, sort
    run -0 jq -Sen "${output}"
    local actual="${output}"

    # handle stdin, quote identifiers, sort
    local expected="$1"
    if [[ "${expected}" == "-" ]]; then
        expected="$(cat)"
    fi
    run -0 jq -Sn "${expected}"
    expected="${output}"

    #shellcheck disable=SC2016
    run jq -ne --argjson a "${actual}" --argjson b "${expected}" '$a == $b'
    #shellcheck disable=SC2154
    if [[ "${status}" -ne 0 ]]; then
        echo "expect: $(jq -c <<<"${expected}")"
        echo "actual: $(jq -c <<<"${actual}")"
        diff <(echo "${actual}") <(echo "${expected}")
        fail "json does not match"
    fi
    output="${oldout}"
}
export -f assert_json

# Check if there's something on stdin by consuming it. Only use this as a way
# to check if something was passed by mistake, since if you read it, it will be
# incomplete.
is_stdin_empty() {
    if read -r -t 0.1; then
        return 1
    fi
    return 0
}
export -f is_stdin_empty

# remove all installed items and data
hub_purge_all() {
    local CONFIG_DIR
    local itemtype
    CONFIG_DIR=$(dirname "$CONFIG_YAML")
    for itemtype in $(cscli hub types -o raw); do
        rm -rf "$CONFIG_DIR"/"${itemtype:?}"/* "$CONFIG_DIR"/hub/"${itemtype:?}"/*
    done
    local DATA_DIR
    DATA_DIR=$(config_get .config_paths.data_dir)
    # should remove everything except the db (find $DATA_DIR -not -name "crowdsec.db*" -delete),
    # but don't play with fire if there is a misconfiguration
    rm -rfv "$DATA_DIR"/GeoLite*
}
export -f hub_purge_all

# remove color and style sequences from stdin
plaintext() {
    sed -E 's/\x1B\[[0-9;]*[JKmsu]//g'
}
export -f plaintext

# like run but defaults to separate stderr and stdout
rune() {
    run --separate-stderr "$@"
}
export -f rune

# call the lapi through unix socket
# the path (and query string) must be the first parameter, the others will be passed to curl
curl-socket() {
    [[ -z "$1" ]] && { fail "${FUNCNAME[0]}: missing path"; }
    local path=$1
    shift
    local socket
    socket=$(config_get '.api.server.listen_socket')
    [[ -z "$socket" ]] && { fail "${FUNCNAME[0]}: missing .api.server.listen_socket"; }
    # curl needs a fake hostname when using a unix socket
    curl --unix-socket "$socket" "http://lapi$path" "$@"
}
export -f curl-socket

# call the lapi through tcp
# the path (and query string) must be the first parameter, the others will be passed to curl
curl-tcp() {
    [[ -z "$1" ]] && { fail "${FUNCNAME[0]}: missing path"; }
    local path=$1
    shift
    local cred
    cred=$(config_get .api.client.credentials_path)
    local base_url
    base_url="$(yq '.url' < "$cred")"
    curl "$base_url$path" "$@"
}
export -f curl-tcp

# call the lapi through unix socket with an API_KEY (authenticates as a bouncer)
# after $1, pass throught extra arguments to curl
curl-with-key() {
    [[ -z "$API_KEY" ]] && { fail "${FUNCNAME[0]}: missing API_KEY"; }
    curl-tcp "$@" -sS --fail-with-body -H "X-Api-Key: $API_KEY"
}
export -f curl-with-key

# call the lapi through unix socket with a TOKEN (authenticates as a machine)
# after $1, pass throught extra arguments to curl
curl-with-token() {
    [[ -z "$TOKEN" ]] && { fail "${FUNCNAME[0]}: missing TOKEN"; }
    # curl needs a fake hostname when using a unix socket
    curl-tcp "$@" -sS --fail-with-body -H "Authorization: Bearer $TOKEN"
}
export -f curl-with-token

# as a log processor, connect to lapi and get a token
lp-get-token() {
    local cred
    cred=$(config_get .api.client.credentials_path)
    local resp
    resp=$(yq -oj -I0 '{"machine_id":.login,"password":.password}' < "$cred" | curl-socket '/v1/watchers/login' -s -X POST --data-binary @-)
    if [[ "$(yq -e '.code' <<<"$resp")" != 200 ]]; then
        echo "login_lp: failed to login" >&3
        return 1
    fi
    echo "$resp" | yq -r '.token'
}
export -f lp-get-token

case $(uname) in
    "Linux")
        # shellcheck disable=SC2089
        RELOAD_MESSAGE="Run 'sudo systemctl reload crowdsec' for the new configuration to be effective."
        ;;
    *)
        # shellcheck disable=SC2089
        RELOAD_MESSAGE="Run 'sudo service crowdsec reload' for the new configuration to be effective."
        ;;
esac

# shellcheck disable=SC2090
export RELOAD_MESSAGE

strip_ansi() {
    local input="$1"
    echo "$input" | sed -E 's/\x1b\[[0-9;]*m//g'
}
export -f strip_ansi
