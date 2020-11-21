apiVersion: apps/v1
kind: Deployment
metadata:
  name: producer-deployment
spec:
  selector:
    matchLabels:
      app: producer
  replicas: 2
  template:
    metadata:
      labels:
        app: producer
    spec:
      containers:
        - name: producer
          image: gcr.io/myproject/producer:latest
          imagePullPolicy: Always
          env:
            - name: PODNAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
          args:
            - --redisHost=1.2.3.4
            - --host=0.0.0.0
            - --id=$(PODNAME)
          ports:
            - containerPort: 8080

---
apiVersion: v1
kind: Service
metadata:
  name: producer-service
spec:
  selector:
    app: producer
  ports:
    - protocol: TCP
      port: 80
      targetPort: 8080
  type: LoadBalancer
  sessionAffinity: ClientIP