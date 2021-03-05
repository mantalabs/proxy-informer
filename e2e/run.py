#!/usr/bin/env python3
import argparse
import atexit
import json
import os
import re
import subprocess
import time


def main(delete_cluster=None,
         create_cluster=None,
         cluster_name=None,
         docker_build=None,
         kind_config=None,
         kind_image=None,
         kubeconfig=None,
         timeout=None):

    def kubectl(*args, return_output=False, json_output=False):
        args = ['kubectl', *args]
        env = {**os.environ, 'KUBECONFIG': kubeconfig}
        if json_output:
            args.extend(['-o', 'json'])
            output = subprocess.check_output(args, env=env)
            return json.loads(output)
        elif return_output:
            return subprocess.check_output(args, env=env).decode('utf-8')

        return subprocess.check_call(args, env=env)

    def kind(*args):
        args = ['kind', *args, '--name', cluster_name]
        env = {**os.environ, 'KUBECONFIG': kubeconfig}
        subprocess.check_call(args, env=env)

    def atexit_delete_cluster():
        try:
            kind('delete', 'cluster')
        except subprocess.CalledProcessError as error:
            print('Failed to delete cluster', error)

    #
    # Setup environment for e2e
    #
    if delete_cluster:
        atexit.register(atexit_delete_cluster)

    if create_cluster:
        kind('create', 'cluster',
             '--verbosity', '4',
             '--config', kind_config,
             '--image', kind_image)

    if docker_build:
        subprocess.check_call([
            'docker', 'build',
            '-t', 'mantalabs/proxy-informer:e2e', '.'])
        kind('load', 'docker-image', 'mantalabs/proxy-informer:e2e')

    kubectl('apply', '-f', 'e2e/pod.yaml')
    kubectl('apply', '-f', 'e2e/rbac.yaml')
    kubectl('apply', '-f', 'e2e/statefulset-tests.yaml')

    started_at = time.time()
    timeout_at = started_at + timeout
    while True:
        try:
            elector_log = kubectl('logs', 'validator-0', 'informer', return_output=True)
            # Indication that the informer is communicating with the validator
            assert re.search('Adding proxy', elector_log)
            assert re.search('Removing proxy', elector_log)
            break
        except (AssertionError, subprocess.CalledProcessError) as error:
            if time.time() < timeout_at:
                sleep_seconds = 5
                print(f'Waiting {sleep_seconds} seconds...')
                time.sleep(sleep_seconds)
                continue

            print('\nTimeout out or assertion failed!\n', error)
            kubectl('describe', 'statefulset/validator')
            raise error

    print(f'\nSuccess ({time.time() - started_at} seconds)\n')


def parse_args():
    parser = argparse.ArgumentParser('Run an e2e test')
    parser.add_argument('--kind-config',
                        default=os.path.join(os.path.dirname(os.path.realpath(__file__)), 'kind.yaml'))
    parser.add_argument('--kubeconfig',
                        default=os.path.join(os.path.dirname(os.path.realpath(__file__)), '.kubeconfig'))
    parser.add_argument('--kind-image',
                        default='kindest/node:v1.16.15')
    parser.add_argument('--delete-cluster',
                        dest='delete_cluster',
                        action='store_true')
    parser.add_argument('--no-delete-cluster',
                        dest='delete_cluster',
                        action='store_false')
    parser.add_argument('--create-cluster',
                        dest='create_cluster',
                        action='store_true')
    parser.add_argument('--no-create-cluster',
                        dest='create_cluster',
                        action='store_false')
    parser.add_argument('--cluster-name',
                        default='proxy-informer-cluster')
    parser.add_argument('--docker-build',
                        dest='docker_build',
                        action='store_true')
    parser.add_argument('--no-docker-build',
                        dest='docker_build',
                        action='store_false')
    parser.add_argument('--timeout',
                        default=90,
                        type=float)

    parser.set_defaults(
        delete_cluster=True,
        create_cluster=True,
        docker_build=True,
    )

    return parser.parse_args()


if __name__ == '__main__':
    main(**vars(parse_args()))
