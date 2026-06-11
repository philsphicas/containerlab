*** Settings ***
Library             OperatingSystem
Resource            ../common.robot

Suite Teardown      Run Keyword    Cleanup


*** Variables ***
${lab-name}         03-04-ceos-intfmap
${lab-file-name}    04-ceos-intfmap-clab.yml
${runtime}          docker


*** Test Cases ***
Deploy ${lab-name} lab
    Log    ${CURDIR}
    ${rc}    ${output} =    Run And Return Rc And Output
    ...    ${CLAB_BIN} --runtime ${runtime} deploy -t ${CURDIR}/${lab-file-name}
    Log    ${output}
    Should Be Equal As Integers    ${rc}    0

Ensure interface mapping file was generated for n1
    ${f} =    OperatingSystem.Get File
    ...    ${CURDIR}/clab-${lab-name}/n1/flash/EosIntfMapping.json
    Log    ${f}
    # eth0 must be remapped to the requested management interface
    Should Contain    ${f}    "eth0": "Management1"
    # the eth1_1 breakout shorthand must be preserved as Ethernet1/1
    Should Contain    ${f}    "eth1_1": "Ethernet1/1"

Verify n1 exposes the management interface as Management1
    ${rc}    ${output} =    Run And Return Rc And Output
    ...    ${CLAB_BIN} --runtime ${runtime} exec -t ${CURDIR}/${lab-file-name} --label clab-node-name\=n1 --cmd "Cli -p 15 -c 'show ip interface brief'"
    Log    ${output}
    Should Be Equal As Integers    ${rc}    0
    Should Contain    ${output}    Management1
    Should Not Contain    ${output}    Management0

Verify n1 exposes the breakout data interface as Ethernet1/1
    ${rc}    ${output} =    Run And Return Rc And Output
    ...    ${CLAB_BIN} --runtime ${runtime} exec -t ${CURDIR}/${lab-file-name} --label clab-node-name\=n1 --cmd "Cli -p 15 -c 'show interfaces description'"
    Log    ${output}
    Should Be Equal As Integers    ${rc}    0
    Should Contain    ${output}    Et1/1

Ensure the management address is assigned to Management1
    ${n1-mgmt-ip} =    Run
    ...    sudo docker inspect clab-${lab-name}-n1 -f '{{range.NetworkSettings.Networks}}{{.IPAddress}}{{end}}'
    Log    ${n1-mgmt-ip}
    Should Not Be Empty    ${n1-mgmt-ip}
    ${rc}    ${output} =    Run And Return Rc And Output
    ...    ${CLAB_BIN} --runtime ${runtime} exec -t ${CURDIR}/${lab-file-name} --label clab-node-name\=n1 --cmd "Cli -p 15 -c 'show ip interface brief'"
    Log    ${output}
    Should Be Equal As Integers    ${rc}    0
    # the docker-assigned mgmt IP must land on Management1, not some other interface
    Should Match Regexp    ${output}    Management1\\s+${n1-mgmt-ip}


*** Keywords ***
Cleanup
    Run    ${CLAB_BIN} --runtime ${runtime} destroy -t ${CURDIR}/${lab-file-name} --cleanup
    Run    rm -rf ${CURDIR}/${lab-name}
