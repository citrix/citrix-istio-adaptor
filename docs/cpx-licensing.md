# Licensing

For licensing Citrix ADC CPX, you need to provide the following information in the YAML file. This information is required for automatically picking the licensing information. The license server runs in the [Citrix ADM](https://docs.citrix.com/en-us/citrix-application-delivery-management-service.html).

-  **LS_IP (License Server IP)** – Specify the License Server IP address(for example: Citrix ADM IP address).

-  **LS_PORT (License Server Port)** – Specifying the License Server port is not mandatory. Specify the ADM port only if you have changed the port. The default port is 27000.



The following is a snippet deployment YAML file:

```yml
apiVersion: extensions/v1beta1
kind: Deployment
metadata:
  name: citrix-ingressgateway
  labels:
    app: citrix-ingressgateway
spec:
...
      - name: ingressgateway
        image: quay/citrix/citrix-k8s-cpx-ingress:13.0-47.22
        imagePullPolicy: IfNotPresent
        securityContext:
          privileged: true
        env:
        - name: "EULA"
          value: "yes"
        - name: "NS_CPX_LITE"
          value: 1
        - name: "KUBERNETES_TASK_ID"
          value: ""
        # Provide the Citrix Application Delivery Management (ADM) IP address and Port to license Citrix ADC CPX. Default port is 27000
        - name: "LS_IP"
          value: ""
        - name: "LS_PORT"
          value: "27000" 
...
---
```    

The complete YAML file can be found [here](https://github.com/citrix/citrix-istio-adaptor/blob/master/deployment/cpx-ingressgateway.tmpl).
